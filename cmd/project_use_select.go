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
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

// 'scion use <project>' across hubs: the known-project list first (offline
// when the name is unique there), then a lookup on every candidate hub, and
// a choice when several hubs have the project.

// useHubTimeout bounds each hub call of 'scion use <project>': the check of
// a known project, and the lookup on each hub.
var useHubTimeout = 5 * time.Second

// useChooseProject asks the user to pick one of several projects. It is a
// variable so tests can substitute it.
var useChooseProject = hubsync.ChooseOption

// useConfirm asks a yes/no question. It is a variable so tests can
// substitute it.
var useConfirm = hubsync.ConfirmAction

// useLoginToHub logs in to the hub at endpoint in place, for the login
// offer of 'scion use <project>'. It stores the credential and nothing
// else: no settings are written. The hub was picked by the lookup, so
// unless it is the effective hub its login client gets only the transport
// of its own hub list entry (see fanOutTransport). It is a variable so
// tests can substitute it.
var useLoginToHub = func(cmd *cobra.Command, endpoint string) error {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Authenticating with Hub at %s\n", endpoint)
	opts, err := useLoginClientOptions(endpoint)
	if err != nil {
		return err
	}
	tokenResp, err := hubAuthLoginFlow(cmd, endpoint, "", opts...)
	if err != nil {
		return err
	}
	return storeTokenAndPrintResult(cmd.OutOrStdout(), endpoint, tokenResp)
}

// useLoginClientOptions returns the client options of the login offer's
// OAuth client: none for the effective hub (transport auth from the
// environment applies, as for 'hub auth login'), else only the transport of
// the hub's own entry.
func useLoginClientOptions(endpoint string) ([]hubclient.Option, error) {
	settings, err := config.LoadSettings(useSettingsPath())
	if err != nil {
		settings = &config.Settings{}
	}
	if hubGetsFullChain(settings, endpoint, false) {
		return nil, nil
	}
	src, mode, err := fanOutTransport(endpoint)
	if err != nil {
		return nil, err
	}
	return []hubclient.Option{hubclient.WithTransportAuth(src, mode)}, nil
}

// useInteractive reports whether 'scion use' may ask the user to choose:
// not with --non-interactive (also implied in agent mode), and only on a
// terminal. --yes alone does not make a choice.
func useInteractive() bool {
	return !nonInteractive && isInteractiveTerminal()
}

// useHub is a hub that 'scion use <project>' searches.
type useHub struct {
	// Name is the hub nickname, or "" for an endpoint with no hub entry.
	Name     string
	Endpoint string
}

// label is how the hub is named in messages and in --hub: its nickname,
// else its URL.
func (h useHub) label() string {
	if h.Name != "" {
		return h.Name
	}
	return h.Endpoint
}

// useMatch is a project on a hub.
type useMatch struct {
	Hub  useHub
	ID   string
	Slug string
	Name string
}

// shown is the project's display name: its slug, else its name.
func (m useMatch) shown() string {
	if m.Slug != "" {
		return m.Slug
	}
	return m.Name
}

// useHubFailure is a hub that could not be searched, and why.
type useHubFailure struct {
	Hub    useHub
	Reason string
}

// useLookup carries the state of one 'scion use <project>' resolution:
// what to write back to the known-project list, and the hubs that could
// not be searched.
type useLookup struct {
	ctx      context.Context
	settings *config.Settings
	ref      string
	// explicit is true when the user chose the hub to search (--hub,
	// SCION_HUB_ENDPOINT or SCION_HUB_URL).
	explicit bool

	// found are the projects seen on named hubs, merged into the known
	// list when the context is saved.
	found map[string][]config.KnownProject
	// dropped are known entries (hub, ID) that their hub reported gone.
	dropped [][2]string
	// notLoggedIn are hubs skipped for want of a usable credential, and
	// hubs that refused the credential sent.
	notLoggedIn []useHub
	// refused are the endpoints of the hubs in notLoggedIn that refused
	// the credential sent (rather than having none to send).
	refused map[string]bool
	// failed are hubs whose lookup failed.
	failed []useHubFailure
	// searched are hubs that answered the lookup.
	searched []useHub
}

// useCandidateHubs returns the hubs to search: only the hub given with
// --hub, or else SCION_HUB_ENDPOINT/SCION_HUB_URL, when one is set
// (explicit is then true); else every registered hub, plus the effective
// hub from the settings when it has no hub entry.
func useCandidateHubs(c *config.CLIContext, settings *config.Settings) (hubs []useHub, explicit bool, err error) {
	chosen := hubFlagEndpoint()
	if chosen == "" {
		chosen, _ = envHubEndpoint()
	}
	if chosen != "" {
		if name, h := c.HubByEndpoint(chosen); h != nil {
			return []useHub{{Name: name, Endpoint: h.Endpoint}}, true, nil
		}
		// Selecting adds a hub entry for it, so it must be a valid hub
		// URL (no user info, query or fragment).
		if err := config.ValidateHubURL(chosen); err != nil {
			return nil, false, err
		}
		return []useHub{{Endpoint: chosen}}, true, nil
	}
	for _, name := range c.HubNames() {
		hubs = append(hubs, useHub{Name: name, Endpoint: c.Hubs[name].Endpoint})
	}
	if ep := GetHubEndpoint(settings); ep != "" {
		if _, h := c.HubByEndpoint(ep); h == nil {
			if err := config.ValidateHubURL(ep); err != nil {
				return nil, false, err
			}
			hubs = append(hubs, useHub{Endpoint: ep})
		}
	}
	if len(hubs) == 0 {
		return nil, false, errHubNotConfigured
	}
	return hubs, false, nil
}

// fanOut reports whether hub must get only credentials bound to it: unless
// the user chose it or it is the effective hub (see hubGetsFullChain).
func (l *useLookup) fanOut(hub useHub) bool {
	return !hubGetsFullChain(l.settings, hub.Endpoint, l.explicit)
}

// remember records projects seen on hub for the known-project list.
func (l *useLookup) remember(hub useHub, projects ...hubclient.Project) {
	if hub.Name == "" {
		return
	}
	if l.found == nil {
		l.found = map[string][]config.KnownProject{}
	}
	for _, p := range projects {
		l.found[hub.Name] = append(l.found[hub.Name], config.KnownProject{ID: p.ID, Slug: p.Slug, Name: p.Name})
	}
}

// isHubGlobalProject reports whether p is a hub's built-in global project.
// Every hub has one, so it is left out of the projects across hubs: of the
// known-project list, of 'scion project list' and of the projects 'scion
// use' can select. The global project on this machine is a directory and
// is not affected.
func isHubGlobalProject(p hubclient.Project) bool {
	return p.Labels[store.LabelGlobalProject] == "true"
}

// withoutHubGlobalProjects returns projects without the hub's global
// project.
func withoutHubGlobalProjects(projects []hubclient.Project) []hubclient.Project {
	var kept []hubclient.Project
	for _, p := range projects {
		if !isHubGlobalProject(p) {
			kept = append(kept, p)
		}
	}
	return kept
}

// useVerdict is the outcome of checking a known project on its hub.
type useVerdict int

const (
	// useVerified: the hub has the project.
	useVerified useVerdict = iota
	// useGone: the hub reported the project not found.
	useGone
	// useUnchecked: the hub could not be asked; the project is selected
	// anyway, with the returned note.
	useUnchecked
)

// verify checks the known project m on its hub with one call by ID. It
// fills in the hub's current slug and name.
func (l *useLookup) verify(m *useMatch) (useVerdict, string) {
	notLoggedIn := fmt.Sprintf("Not logged in to hub '%s'. Run: scion hub auth login --hub %s", m.Hub.label(), m.Hub.label())
	// The hub comes from the known list, not from the user: the full
	// credential chain only for the chosen or effective hub.
	fanOut := l.fanOut(m.Hub)
	if !hubUsable(l.settings, m.Hub.Endpoint, fanOut) {
		return useUnchecked, notLoggedIn
	}
	client, err := hubClientFor(l.settings, m.Hub.Endpoint, fanOut)
	if err != nil {
		return useUnchecked, fmt.Sprintf("Warning: could not check project '%s' on hub '%s': %v", m.shown(), m.Hub.label(), err)
	}
	ctx, cancel := context.WithTimeout(l.ctx, useHubTimeout)
	defer cancel()
	p, err := client.Projects().Get(ctx, m.ID)
	switch {
	case err == nil && isHubGlobalProject(*p):
		// An entry recorded before the hub's global project was left out
		// of the known list.
		l.dropped = append(l.dropped, [2]string{m.Hub.Name, m.ID})
		return useGone, ""
	case err == nil:
		m.Slug, m.Name = p.Slug, p.Name
		l.remember(m.Hub, *p)
		return useVerified, ""
	case apiclient.IsNotFoundError(err):
		l.dropped = append(l.dropped, [2]string{m.Hub.Name, m.ID})
		return useGone, ""
	case apiclient.IsUnauthorizedError(err):
		return useUnchecked, notLoggedIn
	default:
		return useUnchecked, fmt.Sprintf("Warning: could not check project '%s' on hub '%s': %v", m.shown(), m.Hub.label(), hubclient.HintProxyError(err))
	}
}

// lookupOn looks the reference up on one hub. A reference that matches
// nothing is no error; several projects with the name are all returned.
// The hub's global project is never returned. In a fan-out the hub gets
// only a credential bound to it (see hubClientFor).
func (l *useLookup) lookupOn(hub useHub, fanOut bool) ([]hubclient.Project, error) {
	projects, err := l.lookupAllOn(hub, fanOut)
	return withoutHubGlobalProjects(projects), err
}

// lookupAllOn is lookupOn with the hub's global project included.
func (l *useLookup) lookupAllOn(hub useHub, fanOut bool) ([]hubclient.Project, error) {
	client, err := hubClientFor(l.settings, hub.Endpoint, fanOut)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(l.ctx, useHubTimeout)
	defer cancel()
	if util.IsGitURL(l.ref) {
		resp, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{GitRemote: util.NormalizeGitRemote(l.ref)})
		if err != nil {
			return nil, err
		}
		return resp.Projects, nil
	}
	p, err := hubsync.ResolveProjectOnHub(ctx, client, l.ref)
	var notFound *hubsync.ProjectNotFoundError
	var ambiguous *hubsync.AmbiguousProjectError
	switch {
	case errors.As(err, &notFound):
		return nil, nil
	case errors.As(err, &ambiguous):
		return ambiguous.Projects, nil
	case err != nil:
		return nil, err
	}
	return []hubclient.Project{*p}, nil
}

// search looks the reference up on hubs in parallel, each within
// useHubTimeout. Hubs without a usable credential are skipped and not
// contacted; for a hub that is neither chosen nor effective (see
// useLookup.fanOut), that is a hub without a credential bound to it. It
// returns the matches in the order of hubs.
func (l *useLookup) search(hubs []useHub) []useMatch {
	type result struct {
		projects []hubclient.Project
		err      error
		skipped  bool
	}
	results := make([]result, len(hubs))
	var wg sync.WaitGroup
	for i, h := range hubs {
		fanOut := l.fanOut(h)
		if !hubUsable(l.settings, h.Endpoint, fanOut) {
			results[i].skipped = true
			continue
		}
		wg.Add(1)
		go func(i int, h useHub, fanOut bool) {
			defer wg.Done()
			results[i].projects, results[i].err = l.lookupOn(h, fanOut)
		}(i, h, fanOut)
	}
	wg.Wait()

	var matches []useMatch
	for i, h := range hubs {
		r := results[i]
		switch {
		case r.skipped:
			l.notLoggedIn = append(l.notLoggedIn, h)
		case apiclient.IsUnauthorizedError(r.err):
			l.notLoggedIn = append(l.notLoggedIn, h)
			if l.refused == nil {
				l.refused = map[string]bool{}
			}
			l.refused[h.Endpoint] = true
		case r.err != nil:
			l.failed = append(l.failed, useHubFailure{Hub: h, Reason: fmt.Sprintf("unreachable: %v", hubclient.HintProxyError(r.err))})
		default:
			l.searched = append(l.searched, h)
			l.remember(h, r.projects...)
			for _, p := range r.projects {
				matches = append(matches, useMatch{Hub: h, ID: p.ID, Slug: p.Slug, Name: p.Name})
			}
		}
	}
	return matches
}

// skipped describes the hubs that could not be searched, for example
// "staging: not logged in; prod: login refused; dev: unreachable: ...", or
// "".
func (l *useLookup) skipped() string {
	var parts []string
	for _, h := range l.notLoggedIn {
		reason := "not logged in"
		if l.refused[h.Endpoint] {
			reason = "login refused"
		}
		parts = append(parts, h.label()+": "+reason)
	}
	for _, f := range l.failed {
		parts = append(parts, f.Hub.label()+": "+f.Reason)
	}
	return strings.Join(parts, "; ")
}

// notFoundError is the error when no searched hub has the project. It
// names the hubs that were searched and those that could not be: the
// project may be on one of those. It ends with the login command for each
// hub that was skipped for want of a login or refused it.
func (l *useLookup) notFoundError() error {
	var names []string
	for _, h := range l.searched {
		names = append(names, h.label())
	}
	var b strings.Builder
	switch skipped := l.skipped(); {
	case len(names) > 0 && skipped != "":
		fmt.Fprintf(&b, "project '%s' not found on the hubs searched (%s); it may be on the hubs that could not be searched (%s)",
			l.ref, strings.Join(names, ", "), skipped)
	case len(names) > 0:
		fmt.Fprintf(&b, "project '%s' not found on hubs: %s", l.ref, strings.Join(names, ", "))
	default:
		fmt.Fprintf(&b, "project '%s' could not be looked up (%s)", l.ref, l.skipped())
	}
	for i, h := range l.notLoggedIn {
		if i == 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "\nLog in with: scion hub auth login --hub %s", h.label())
	}
	return errors.New(b.String())
}

// apply writes what the lookup learned into the known-project list: the
// entries proven gone are dropped and the projects seen are merged.
func (l *useLookup) apply(c *config.CLIContext) {
	for _, d := range l.dropped {
		c.DropKnownProject(d[0], d[1])
	}
	names := make([]string, 0, len(l.found))
	for name := range l.found {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if c.Hubs[name] != nil {
			c.RememberProjects(name, l.found[name])
		}
	}
}

// learned reports whether apply has anything to write.
func (l *useLookup) learned() bool {
	return len(l.dropped) > 0 || len(l.found) > 0
}

// saveLearned writes what the lookup learned when no project is selected.
// It is best effort: the command fails anyway, with its own error.
func (l *useLookup) saveLearned() {
	if !l.learned() {
		return
	}
	_ = updateUseContext(func(c *config.CLIContext) error {
		l.apply(c)
		return nil
	})
}

// runUseSelect is 'scion use <project>': it finds the project on the
// candidate hubs, lets the user choose when several hubs have it, and saves
// it as the current project of its hub, which becomes the current hub.
func runUseSelect(cmd *cobra.Command, ref string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	settings, err := config.LoadSettings(useSettingsPath())
	if err != nil {
		settings = &config.Settings{}
	}
	c, _ := config.LoadCLIContext()
	hubs, explicit, err := useCandidateHubs(c, settings)
	if err != nil {
		return err
	}
	l := &useLookup{ctx: ctx, settings: settings, ref: ref, explicit: explicit}

	var chosen *useMatch
	var candidates []useMatch
	var checkNote string
	if !useRefresh {
		named := map[string]bool{}
		for _, h := range hubs {
			if h.Name != "" {
				named[h.Name] = true
			}
		}
		known := c.MatchKnownProjects(ref, named)
		switch {
		case len(known) == 1:
			m := useMatch{Hub: useHub{Name: known[0].Hub, Endpoint: c.Hubs[known[0].Hub].Endpoint},
				ID: known[0].ID, Slug: known[0].Slug, Name: known[0].Name}
			verdict, note := l.verify(&m)
			if verdict != useGone {
				chosen, checkNote = &m, note
			}
		case len(known) > 1:
			for _, k := range known {
				candidates = append(candidates, useMatch{Hub: useHub{Name: k.Hub, Endpoint: c.Hubs[k.Hub].Endpoint},
					ID: k.ID, Slug: k.Slug, Name: k.Name})
			}
		}
	}

	if chosen == nil && candidates == nil {
		candidates = l.search(hubs)
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(candidates) == 0 && len(l.notLoggedIn) == 1 && useInteractive() && !isJSONOutput() {
			h := l.notLoggedIn[0]
			// --yes does not start a login: opening a browser is never
			// implied.
			if useConfirm(fmt.Sprintf("Log in to %s now?", h.label()), true, false) {
				if err := useLoginToHub(cmd, h.Endpoint); err != nil {
					return err
				}
				l.notLoggedIn, l.refused = nil, nil
				candidates = l.search([]useHub{h})
			}
		}
		if s := l.skipped(); s != "" && len(candidates) > 0 {
			_, _ = fmt.Fprintf(stderr, "Warning: skipped hubs: %s\n", s)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if chosen == nil {
		switch len(candidates) {
		case 0:
			l.saveLearned()
			return l.notFoundError()
		case 1:
			chosen = &candidates[0]
		default:
			m, err := useChoose(ref, candidates)
			if err != nil {
				l.saveLearned()
				return err
			}
			chosen = m
		}
	}
	return saveUseSelection(out, stderr, l, *chosen, checkNote)
}

// sortUseCandidates puts candidates in the order every listing of them
// uses (the picker, the error and its JSON), whether they came from the
// known-project list or from a lookup: by hub, then by project ID.
func sortUseCandidates(candidates []useMatch) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Hub.label() != b.Hub.label() {
			return a.Hub.label() < b.Hub.label()
		}
		return a.ID < b.ID
	})
}

// useChoose picks one of several candidates: with the picker when the user
// can be asked, else it fails with the candidates and how to choose one.
// It lists them in the order of sortUseCandidates.
func useChoose(ref string, candidates []useMatch) (*useMatch, error) {
	sortUseCandidates(candidates)
	if useInteractive() && !isJSONOutput() {
		options := make([]string, len(candidates))
		for i, m := range candidates {
			options[i] = fmt.Sprintf("%s on %s (%s)", m.shown(), m.Hub.label(), m.Hub.Endpoint)
		}
		i, err := useChooseProject(fmt.Sprintf("Several projects match '%s':", ref), options)
		switch {
		case errors.Is(err, hubsync.ErrNoTerminal):
			return nil, useAmbiguousError(ref, candidates)
		case err != nil:
			return nil, err
		case i < 0 || i >= len(candidates):
			return nil, errors.New("no project selected")
		}
		return &candidates[i], nil
	}
	return nil, useAmbiguousError(ref, candidates)
}

// useAmbiguousError reports several projects matching ref. On one hub they
// are told apart by slug; across hubs, by --hub (and by slug for a hub
// with several of them).
func useAmbiguousError(ref string, candidates []useMatch) error {
	perHub := map[string]int{}
	for _, m := range candidates {
		perHub[m.Hub.Endpoint]++
	}
	oneHub := len(perHub) == 1
	var msg string
	if oneHub {
		msg = fmt.Sprintf("project '%s' matches several projects on the hub at %s", ref, candidates[0].Hub.Endpoint)
	} else {
		msg = fmt.Sprintf("project '%s' exists on several hubs", ref)
	}

	if isJSONOutput() {
		cands := make([]useCandidateJSON, 0, len(candidates))
		for _, m := range candidates {
			cands = append(cands, useCandidateJSON{Hub: m.Hub.Name, Endpoint: m.Hub.Endpoint, ID: m.ID, Slug: m.Slug})
		}
		return outputJSONResult(map[string]interface{}{"error": "ambiguous", "candidates": cands}, true, msg)
	}

	var b strings.Builder
	if oneHub {
		b.WriteString(msg + "; choose one by slug:")
		for _, m := range candidates {
			fmt.Fprintf(&b, "\n  scion use %s   (id %s)", m.Slug, m.ID)
		}
		return errors.New(b.String())
	}
	b.WriteString(msg + "; choose one with --hub:\n")
	w := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	for _, m := range candidates {
		name := ref
		if perHub[m.Hub.Endpoint] > 1 {
			name = m.Slug
		}
		_, _ = fmt.Fprintf(w, "  scion use %s --hub %s\t(%s, id %s)\n", name, m.Hub.label(), m.Hub.Endpoint, m.ID)
	}
	_ = w.Flush()
	return errors.New(strings.TrimSuffix(b.String(), "\n"))
}

// saveUseSelection saves m as the current project of its hub, which
// becomes the current hub, together with what the lookup learned, and
// reports the selection.
func saveUseSelection(out, stderr io.Writer, l *useLookup, m useMatch, checkNote string) error {
	endpoint := m.Hub.Endpoint
	var hubName string
	var registered bool
	err := updateUseContext(func(c *config.CLIContext) error {
		l.apply(c)
		hubName = m.Hub.Name
		if hubName == "" || c.Hubs[hubName] == nil {
			hubName, registered = c.EnsureHub(endpoint, useHubBaseName(endpoint))
		}
		return c.SelectProject(hubName, config.KnownProject{ID: m.ID, Slug: m.Slug, Name: m.Name}, time.Now())
	})
	if err != nil {
		return err
	}

	if isJSONOutput() {
		return outputJSON(useSelectJSON{
			Hub:     useHubJSON{Name: hubName, Endpoint: endpoint, Source: useSourceContext},
			Project: useProjectJSON{ID: m.ID, Slug: m.Slug, Name: m.Name, Source: useSourceContext},
		})
	}
	// Announce a new entry only for a URL given with --hub. An endpoint
	// from the environment or settings gets its entry silently, as before
	// there were hub nicknames.
	if registered && hubFlagEndpoint() != "" {
		printRegisteredHub(out, hubName, endpoint)
	}
	shown := m.shown()
	_, _ = fmt.Fprintf(out, "Now using project '%s' on hub '%s' (%s).\n", shown, hubName, endpoint)
	switch {
	case strings.HasPrefix(checkNote, "Warning: "):
		_, _ = fmt.Fprintln(stderr, checkNote)
	case checkNote != "":
		_, _ = fmt.Fprintln(out, checkNote)
	}
	if hubSource == targetSourceContext {
		warnHubTokenForContextHub(stderr, hubName)
	}
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
