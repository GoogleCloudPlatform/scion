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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

const (
	// sshListPageLimit is the page size for agent and project listings.
	sshListPageLimit = 200
	// sshListMaxPages caps a listing; reaching it counts as truncated.
	sshListMaxPages = 1000
	// sshProbeTimeout bounds the reachability probe of another hub's
	// recorded endpoint.
	sshProbeTimeout = 3 * time.Second
)

var sshConfigWrite bool

var sshConfigCmd = &cobra.Command{
	Use:   "ssh-config [<agent>]",
	Short: "Print or install SSH Host blocks for agents (for example VS Code Remote-SSH)",
	Long: `Print or install SSH Host blocks for reaching agents through scion tunnel.

Each agent gets a Host named scion.<hub>.<project>.<agent> whose ProxyCommand
runs scion tunnel with the hub and project pinned, and whose host key is pinned
in a known-hosts file kept by scion.

With no agent, every agent the hub lists across all projects is included.
With --write, the blocks are installed in ~/.ssh/config.d/scion-<hub>, the host
keys in ~/.scion/ssh/known_hosts-<hub>, and one Include line is added at the
top of ~/.ssh/config. Regenerating without an agent removes the blocks of
agents that are no longer listed; if the listing fails or is incomplete,
nothing is written. With an agent, only that agent's block is updated.

Without --write, the files are printed to stdout and nothing is written.

Exit codes: 0 success, 1 other error, 3 no usable or refused credential,
4 agent not found, 5 not permitted, 6 hub unreachable or unavailable.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		env, err := defaultSSHCommandEnv()
		if err != nil {
			fmt.Fprintf(os.Stderr, "scion ssh-config: %s\n", sshOneLine(err.Error()))
			sshExit(CodeOf(err))
			return nil
		}
		if code := runSSHConfig(cmd.Context(), env, args, sshConfigWrite); code != ExitOK {
			sshExit(code)
		}
		return nil
	},
}

func init() {
	sshConfigCmd.Flags().BoolVar(&sshConfigWrite, "write", false, "Install the include and known-hosts files instead of printing them")
	rootCmd.AddCommand(sshConfigCmd)
}

// sshExit ends the process with code; tests replace it.
var sshExit = os.Exit

// sshCommandEnv is everything ssh and ssh-config read from the environment,
// resolved once so tests can supply it directly.
type sshCommandEnv struct {
	home              string
	exe               string // absolute scion path written into ProxyCommand
	endpoint          string // hub endpoint from GetHubEndpoint
	token             string // user token from getHubAccessToken
	settingsProjectID string // hub project id from local settings, if any
	stdin             io.Reader
	stdout            io.Writer
	stderr            io.Writer
	probe             func(ctx context.Context, endpoint string) error
}

// defaultSSHCommandEnv resolves the endpoint, credential, home directory and
// executable path. It reads local settings only; it does not bootstrap the
// project, sync with the hub or read from a terminal.
func defaultSSHCommandEnv() (*sshCommandEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, Errorf(ExitError, "home directory: %v", err)
	}
	exe, err := sshExecutablePath()
	if err != nil {
		return nil, Errorf(ExitError, "scion executable path: %v", err)
	}
	var settings *config.Settings
	if resolved, _, rerr := config.ResolveProjectPath(projectPath); rerr == nil {
		settings, _ = config.LoadSettings(resolved)
	}
	var endpoint, projectID string
	if settings != nil {
		endpoint = GetHubEndpoint(settings)
		projectID = settings.GetHubProjectID()
		if projectID == "" {
			projectID = settings.ProjectID
		}
	} else {
		endpoint = GetHubEndpoint(nil)
	}
	env := &sshCommandEnv{
		home:              home,
		exe:               exe,
		endpoint:          endpoint,
		settingsProjectID: projectID,
		stdin:             os.Stdin,
		stdout:            os.Stdout,
		stderr:            os.Stderr,
		probe:             probeSSHHubEndpoint,
	}
	if endpoint != "" {
		env.token = getHubAccessToken(endpoint)
	}
	return env, nil
}

// sshExecutablePath returns the absolute path of the running scion binary
// without resolving symlinks, so a package-manager upgrade behind a stable
// symlink keeps working. On Linux os.Executable resolves symlinks, so the
// invoked path (argv[0], looked up on PATH when bare) is preferred when it
// names the same file.
func sshExecutablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	cand := os.Args[0]
	if cand != "" && !strings.ContainsRune(cand, filepath.Separator) {
		if p, lerr := exec.LookPath(cand); lerr == nil {
			cand = p
		} else {
			cand = ""
		}
	}
	if cand != "" {
		if abs, aerr := filepath.Abs(cand); aerr == nil {
			fa, e1 := os.Stat(abs)
			fb, e2 := os.Stat(exe)
			if e1 == nil && e2 == nil && os.SameFile(fa, fb) {
				return abs, nil
			}
		}
	}
	return exe, nil
}

// probeSSHHubEndpoint reports whether endpoint answers HTTP at all. It sends
// no credential and follows no redirect; any HTTP response counts as
// reachable.
func probeSSHHubEndpoint(ctx context.Context, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, sshProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/healthz", nil)
	if err != nil {
		return err
	}
	hc := &http.Client{
		Timeout: sshProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// sshHubTarget is the resolved hub access for one ssh or ssh-config run.
type sshHubTarget struct {
	conn     *sshHubConn
	endpoint string // normalized
	label    string
	files    []sshIncludeFileInfo
}

// resolveSSHHub checks the endpoint and credential, builds the hub access and
// resolves the hub label from the existing include files.
func resolveSSHHub(env *sshCommandEnv) (*sshHubTarget, error) {
	if env.endpoint == "" {
		return nil, Errorf(ExitError, "no hub endpoint configured; use --hub or SCION_HUB_ENDPOINT")
	}
	normalized, err := NormalizeEndpoint(env.endpoint)
	if err != nil {
		return nil, Errorf(ExitUsage, "%v", err)
	}
	u, err := sshEndpointURL(normalized)
	if err != nil {
		return nil, Errorf(ExitUsage, "%v", err)
	}
	if env.token == "" {
		return nil, Errorf(ExitCredential, "no usable credential for hub %s; run scion hub auth login", normalized)
	}
	conn, err := newSSHHubConn(normalized, env.token)
	if err != nil {
		return nil, err
	}
	p := sshPaths{home: env.home}
	files, err := listSSHIncludeFiles(p)
	if err != nil {
		return nil, Errorf(ExitError, "reading %s: %v", p.includeDir(), err)
	}
	label, err := resolveSSHHubLabel(normalized, u, files)
	if err != nil {
		return nil, Errorf(ExitError, "%v", err)
	}
	return &sshHubTarget{conn: conn, endpoint: normalized, label: label, files: files}, nil
}

// sshAgentEntry is one agent with the slugs its Host name is built from.
type sshAgentEntry struct {
	projectID   string
	projectSlug string
	agentID     string
	agentSlug   string
}

// listAllSSHProjects returns every project the hub lists, keyed by id. Any
// failure or incomplete listing is an error.
func (h *sshHubConn) listAllSSHProjects(ctx context.Context) (map[string]hubclient.Project, error) {
	out := map[string]hubclient.Project{}
	cursor := ""
	seen := map[string]bool{}
	total := 0
	for page := 0; ; page++ {
		if page >= sshListMaxPages {
			return nil, sshTruncated(h.endpoint, "project")
		}
		resp, err := h.client.Projects().List(ctx, &hubclient.ListProjectsOptions{
			Page: apiclient.PageOptions{Limit: sshListPageLimit, Cursor: cursor},
		})
		if err != nil {
			return nil, classifySSHHubError(h.endpoint, err)
		}
		for _, p := range resp.Projects {
			out[p.ID] = p
		}
		total = resp.Page.TotalCount
		if !resp.Page.HasMore() {
			break
		}
		if seen[resp.Page.NextCursor] {
			return nil, sshTruncated(h.endpoint, "project")
		}
		seen[resp.Page.NextCursor] = true
		cursor = resp.Page.NextCursor
	}
	if total > len(out) {
		return nil, sshTruncated(h.endpoint, "project")
	}
	return out, nil
}

// listAllSSHAgents returns every agent the hub lists across all projects.
// Any failure or incomplete listing is an error.
func (h *sshHubConn) listAllSSHAgents(ctx context.Context) ([]hubclient.Agent, error) {
	var out []hubclient.Agent
	ids := map[string]bool{}
	cursor := ""
	seen := map[string]bool{}
	total := 0
	for page := 0; ; page++ {
		if page >= sshListMaxPages {
			return nil, sshTruncated(h.endpoint, "agent")
		}
		resp, err := h.client.Agents().List(ctx, &hubclient.ListAgentsOptions{
			Page: apiclient.PageOptions{Limit: sshListPageLimit, Cursor: cursor},
		})
		if err != nil {
			return nil, classifySSHHubError(h.endpoint, err)
		}
		for _, a := range resp.Agents {
			if !ids[a.ID] {
				ids[a.ID] = true
				out = append(out, a)
			}
		}
		total = resp.Page.TotalCount
		if !resp.Page.HasMore() {
			break
		}
		if seen[resp.Page.NextCursor] {
			return nil, sshTruncated(h.endpoint, "agent")
		}
		seen[resp.Page.NextCursor] = true
		cursor = resp.Page.NextCursor
	}
	if total > len(out) {
		return nil, sshTruncated(h.endpoint, "agent")
	}
	return out, nil
}

func sshTruncated(endpoint, kind string) *Error {
	return Errorf(ExitError, "the %s list from hub %s is incomplete; nothing was written", kind, endpoint)
}

// sshProjectSlug returns the slug for projectID from projects, asking the
// hub for a project the listing did not include.
func (h *sshHubConn) sshProjectSlug(ctx context.Context, projects map[string]hubclient.Project, projectID string) (string, error) {
	if p, ok := projects[projectID]; ok && p.Slug != "" {
		return p.Slug, nil
	}
	p, err := h.client.Projects().Get(ctx, projectID)
	if err != nil {
		return "", classifySSHHubError(h.endpoint, err)
	}
	if p == nil || p.Slug == "" {
		return "", Errorf(ExitError, "hub %s returned no slug for project %s", h.endpoint, projectID)
	}
	projects[projectID] = *p
	return p.Slug, nil
}

// resolveSSHTarget resolves an agent reference. "<project>/<agent>" names the
// project; a bare agent uses the project from local settings, or else must
// match exactly one agent across all projects.
func (h *sshHubConn) resolveSSHTarget(ctx context.Context, ref, settingsProjectID string) (sshAgentEntry, error) {
	project, agent, hasProject := strings.Cut(ref, "/")
	if !hasProject {
		agent = ref
		project = settingsProjectID
	}
	if agent == "" || (hasProject && project == "") {
		return sshAgentEntry{}, Errorf(ExitUsage, "invalid agent reference %q; use <agent> or <project>/<agent>", ref)
	}
	if project != "" {
		projectID, projectSlug, err := h.resolveSSHProjectID(ctx, project)
		if err != nil {
			return sshAgentEntry{}, err
		}
		a, err := h.client.ProjectAgents(projectID).Get(ctx, agent)
		if err != nil {
			var apiErr *apiclient.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return sshAgentEntry{}, Errorf(ExitAgentNotFound, "agent not found or deleted: %s/%s", projectSlug, agent)
			}
			return sshAgentEntry{}, classifySSHHubError(h.endpoint, err)
		}
		return sshAgentEntry{projectID: projectID, projectSlug: projectSlug, agentID: a.ID, agentSlug: a.Slug}, nil
	}
	agents, err := h.listAllSSHAgents(ctx)
	if err != nil {
		return sshAgentEntry{}, err
	}
	projects := map[string]hubclient.Project{}
	var matches []sshAgentEntry
	for _, a := range agents {
		if a.Slug != agent {
			continue
		}
		slug, err := h.sshProjectSlug(ctx, projects, a.ProjectID)
		if err != nil {
			return sshAgentEntry{}, err
		}
		matches = append(matches, sshAgentEntry{projectID: a.ProjectID, projectSlug: slug, agentID: a.ID, agentSlug: a.Slug})
	}
	switch len(matches) {
	case 0:
		return sshAgentEntry{}, Errorf(ExitAgentNotFound, "agent not found or deleted: %s", agent)
	case 1:
		return matches[0], nil
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m.projectSlug+"/"+m.agentSlug)
	}
	sort.Strings(names)
	return sshAgentEntry{}, Errorf(ExitUsage, "agent %s exists in several projects (%s); use <project>/<agent>", agent, strings.Join(names, ", "))
}

// sshHubFiles is the generated content for one hub.
type sshHubFiles struct {
	blocks map[string]string // host name -> Host block
	keys   map[string]string // host name -> "<type> <base64>"
}

// runSSHConfig runs scion ssh-config and returns the exit code. Errors are
// printed to env.stderr as one plain line.
func runSSHConfig(ctx context.Context, env *sshCommandEnv, args []string, write bool) int {
	if err := sshConfigMain(ctx, env, args, write); err != nil {
		fmt.Fprintf(env.stderr, "scion ssh-config: %s\n", sshOneLine(err.Error()))
		return CodeOf(err)
	}
	return ExitOK
}

func sshConfigMain(ctx context.Context, env *sshCommandEnv, args []string, write bool) error {
	hub, err := resolveSSHHub(env)
	if err != nil {
		return err
	}
	p := sshPaths{home: env.home}
	existingKeys, err := readKnownHosts(p.knownHostsFile(hub.label))
	if err != nil {
		return Errorf(ExitError, "reading %s: %v", p.knownHostsFile(hub.label), err)
	}
	existingBlocks := map[string]string{}
	if data, rerr := os.ReadFile(p.includeFile(hub.label)); rerr == nil {
		existingBlocks = parseSSHHostBlocks(string(data))
	} else if !errors.Is(rerr, os.ErrNotExist) {
		return Errorf(ExitError, "reading %s: %v", p.includeFile(hub.label), rerr)
	}

	var gen *sshHubFiles
	if len(args) == 1 {
		gen, err = sshConfigOneAgent(ctx, env, hub, args[0], existingBlocks, existingKeys)
	} else {
		gen, err = sshConfigAllAgents(ctx, env, hub, existingKeys)
	}
	if err != nil {
		return err
	}

	include := renderSSHIncludeFile(hub.endpoint, hub.label, gen.blocks)
	known := renderKnownHosts(gen.keys)

	if !write {
		fmt.Fprintf(env.stdout, "# %s\n%s\n# %s\n%s", p.includeFile(hub.label), include, p.knownHostsFile(hub.label), known)
		return nil
	}

	for host, key := range gen.keys {
		if old, ok := existingKeys[host]; ok && old != key {
			fmt.Fprintf(env.stdout, "host key for %s changed (agent was recreated); known_hosts line rewritten\n", host)
		}
	}
	if err := ensureSSHDir(p.home, p.knownHostsDir()); err != nil {
		return Errorf(ExitError, "creating %s: %v", p.knownHostsDir(), err)
	}
	if err := ensureSSHDir(p.home, p.includeDir()); err != nil {
		return Errorf(ExitError, "creating %s: %v", p.includeDir(), err)
	}
	// Known hosts first, so an installed block never lacks its pin.
	if err := writeFileAtomic(p.knownHostsFile(hub.label), []byte(known), sshFileMode); err != nil {
		return Errorf(ExitError, "writing %s: %v", p.knownHostsFile(hub.label), err)
	}
	if err := writeFileAtomic(p.includeFile(hub.label), []byte(include), sshFileMode); err != nil {
		return Errorf(ExitError, "writing %s: %v", p.includeFile(hub.label), err)
	}
	if _, err := ensureSSHIncludeLine(p); err != nil {
		return Errorf(ExitError, "updating %s: %v", p.userConfig(), err)
	}
	fmt.Fprintf(env.stdout, "wrote %s (%d hosts) and %s\n", p.includeFile(hub.label), len(gen.blocks), p.knownHostsFile(hub.label))
	warnUnreachableSSHHubs(ctx, env, hub)
	return nil
}

// warnUnreachableSSHHubs warns, naming the file, about each other hub's
// include file whose recorded endpoint does not answer. It never removes
// the file.
func warnUnreachableSSHHubs(ctx context.Context, env *sshCommandEnv, hub *sshHubTarget) {
	if env.probe == nil {
		return
	}
	for _, f := range hub.files {
		if f.endpoint == "" || f.endpoint == hub.endpoint {
			continue
		}
		if err := env.probe(ctx, f.endpoint); err != nil {
			fmt.Fprintf(env.stderr, "warning: %s: recorded hub endpoint %s cannot be reached; if that hub is gone, remove the file by hand\n", f.path, f.endpoint)
		}
	}
}

// sshConfigOneAgent updates one agent's block and pin and keeps every other
// block and pin as it is.
func sshConfigOneAgent(ctx context.Context, env *sshCommandEnv, hub *sshHubTarget, ref string, existingBlocks, existingKeys map[string]string) (*sshHubFiles, error) {
	ctx, cancel := context.WithTimeout(ctx, sshHubRequestTimeout)
	defer cancel()
	entry, err := hub.conn.resolveSSHTarget(ctx, ref, env.settingsProjectID)
	if err != nil {
		return nil, err
	}
	key, err := hub.conn.agentSSHHostKey(ctx, "", entry.agentID)
	if err != nil {
		return nil, err
	}
	host := sshHostName(hub.label, entry.projectSlug, entry.agentSlug)
	if key == "" {
		return nil, Errorf(ExitError, "agent %s/%s has no SSH host key yet", entry.projectSlug, entry.agentSlug)
	}
	norm, err := normalizeSSHHostKey(key)
	if err != nil {
		return nil, Errorf(ExitError, "agent %s/%s has an invalid SSH host key: %v", entry.projectSlug, entry.agentSlug, err)
	}
	gen := &sshHubFiles{blocks: map[string]string{}, keys: map[string]string{}}
	for k, v := range existingBlocks {
		gen.blocks[k] = v
	}
	for k, v := range existingKeys {
		gen.keys[k] = v
	}
	gen.blocks[host] = renderSSHHostBlock(env.exe, hub.endpoint, hub.label, entry.projectSlug, entry.agentSlug)
	gen.keys[host] = norm
	return gen, nil
}

// sshConfigAllAgents builds every block from a complete listing of the hub.
// Agents that are no longer listed are left out, which prunes them.
func sshConfigAllAgents(ctx context.Context, env *sshCommandEnv, hub *sshHubTarget, existingKeys map[string]string) (*sshHubFiles, error) {
	projects, err := hub.conn.listAllSSHProjects(ctx)
	if err != nil {
		return nil, err
	}
	agents, err := hub.conn.listAllSSHAgents(ctx)
	if err != nil {
		return nil, err
	}
	gen := &sshHubFiles{blocks: map[string]string{}, keys: map[string]string{}}
	for _, a := range agents {
		slug, err := hub.conn.sshProjectSlug(ctx, projects, a.ProjectID)
		if err != nil {
			return nil, err
		}
		host := sshHostName(hub.label, slug, a.Slug)
		if _, dup := gen.blocks[host]; dup {
			return nil, Errorf(ExitError, "two agents map to host %s", host)
		}
		key, err := hub.conn.agentSSHHostKey(ctx, "", a.ID)
		if err != nil {
			if CodeOf(err) == ExitAgentNotFound {
				continue // deleted since the listing
			}
			return nil, err
		}
		norm := ""
		if key != "" {
			norm, err = normalizeSSHHostKey(key)
			if err != nil {
				fmt.Fprintf(env.stderr, "warning: agent %s/%s has an invalid SSH host key; skipped\n", slug, a.Slug)
				continue
			}
		}
		if norm == "" {
			// No key published (for example not started yet). Keep an
			// existing pin rather than dropping a listed agent.
			old, ok := existingKeys[host]
			if !ok {
				fmt.Fprintf(env.stderr, "warning: agent %s/%s has no SSH host key yet; skipped\n", slug, a.Slug)
				continue
			}
			norm = old
		}
		gen.blocks[host] = renderSSHHostBlock(env.exe, hub.endpoint, hub.label, slug, a.Slug)
		gen.keys[host] = norm
	}
	return gen, nil
}
