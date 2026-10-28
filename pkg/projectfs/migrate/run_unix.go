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

//go:build unix

package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// ProjectRef is a hub project as the migration sees it.
type ProjectRef struct {
	ID   string
	Slug string
	// Git: the project is git-backed (has a git remote).
	Git bool
	// LocalBrokerTree: a co-located broker serves this project from the
	// hub-local ~/.scion/projects/<slug> (the per-project provider check);
	// steps 2 and 4 only copy from that tree.
	LocalBrokerTree bool
	// NoWorkspace: the hub manages no project-scoped workspace for the
	// project (empty-per-agent, or a linked project in a per-agent git
	// mode). Steps 1 and 2 then leave legacy content in place, reported.
	NoWorkspace bool
	// Agents are the project's agents, for step 5 (Cloud Run homes).
	Agents []AgentRef
}

// AgentRef is an agent as step 5 sees it: its hub ID, its slug, and
// whether it is stopped (only a stopped agent's home is moved).
type AgentRef struct {
	ID      string
	Slug    string
	Stopped bool
}

// HubInput is one hub's the migration/the object-storage cleanup run.
type HubInput struct {
	// Workspace is the hub's workspace Layout (targets of steps 1, 2, 4,
	// and the legacy hub-managed root).
	Workspace projectfs.Layout
	// SharedDirs is the hub's shared-dir Layout. Step 3 runs only when it
	// is a shared backend.
	SharedDirs projectfs.Layout
	// GlobalDir is the hub's own global Scion dir (~/.scion).
	GlobalDir string
	Projects  []ProjectRef
	// Objects is the hub's object storage (nil: the object-storage cleanup is skipped).
	Objects ObjectStore
	// ObjectPrefix is the directory prefix of a project's legacy GCS
	// workspace copy (the object-storage cleanup).
	ObjectPrefix func(projectID string) string
	// ProvisionLock, when set, runs section holding every provisioning lock
	// a provisioner of the project workspace at workspace may take (the hub
	// creates the state directory with the leaf contract and passes
	// provision.WithWorkspaceProvisionLocks, which derives the lock
	// directories the way ProvisionShared does). The section gets the
	// locks' context, cancelled if any lock is lost.
	ProvisionLock func(workspace Tree, section func(lockCtx context.Context) error) error
	// Tracked overrides GitTracked (tests).
	Tracked func(ctx context.Context, dir, path string) (bool, error)
	// AgentStopped, when set, re-reads whether an agent is still stopped,
	// immediately before step 5 moves its home (the ProjectRef snapshot can
	// be stale by then). An agent that is no longer stopped is deferred.
	AgentStopped func(ctx context.Context, projectID, agentID string) (bool, error)
}

// HubReport is one hub's the migration/the object-storage cleanup report.
type HubReport struct {
	DryRun bool
	// Skipped is set when the run did nothing at all (the workspace
	// backend is not shared: the workstation server and single-node
	// Cloud Run keep today's layout).
	Skipped  string
	Projects []ProjectReport
	Objects  []ObjectResult
}

// Moves counts the moves (or planned moves) over all projects.
func (h HubReport) Moves() int {
	n := 0
	for _, p := range h.Projects {
		n += p.Moves()
	}
	return n
}

// Failed reports whether any project step or object cleanup failed.
func (h HubReport) Failed() bool {
	for _, p := range h.Projects {
		if p.Failed() {
			return true
		}
	}
	for _, o := range h.Objects {
		if o.Outcome == ObjectsFailed {
			return true
		}
	}
	return false
}

// PendingProjects returns the IDs of projects with legacy content left:
// something to move, or a step that failed (content that could not be
// moved stays in its legacy tree).
func (h HubReport) PendingProjects() []string {
	var ids []string
	for _, p := range h.Projects {
		if p.Moves() > 0 || p.Failed() {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// IdentityBlockedProjects returns the IDs of projects with a legacy tree
// that failed the identity check (mismatch or unverified).
func (h HubReport) IdentityBlockedProjects() []string {
	var ids []string
	for _, p := range h.Projects {
		for _, r := range p.Results {
			if r.Step == StepIdentity && r.Failed() {
				ids = append(ids, p.ID)
				break
			}
		}
	}
	return ids
}

// Run runs (or plans) the migration for every project of a hub, then the object-storage cleanup per project.
// On a hub whose workspace backend is not shared it touches nothing and
// reports Skipped. A shared backend without a usable mount root fails
// closed with an error.
func Run(ctx context.Context, in HubInput, opts Options) (HubReport, error) {
	rep := HubReport{DryRun: opts.DryRun}
	if opts.Context == nil {
		opts.Context = ctx
	}
	wsShared, sdShared := in.Workspace.Backend.Shared(), in.SharedDirs.Backend.Shared()
	if !wsShared && !sdShared {
		rep.Skipped = fmt.Sprintf("workspace backend %q and shared-dir backend %q are not shared; nothing to migrate", in.Workspace.Backend, in.SharedDirs.Backend)
		return rep, nil
	}
	var legacy Tree
	if wsShared {
		var err error
		if legacy, err = LegacyHubProjectsRoot(in.Workspace); err != nil {
			return rep, err
		}
	}
	if in.GlobalDir == "" {
		return rep, errors.New("global dir is required")
	}
	for _, ref := range in.Projects {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		pi, err := projectInput(in, legacy, ref)
		var pr ProjectReport
		if err != nil {
			pr = ProjectReport{ID: ref.ID, Slug: ref.Slug, Results: []Result{{Step: StepSeed, Outcome: OutcomeFailed, Err: err}}}
		} else {
			pr = MigrateProject(ctx, pi, opts)
		}
		rep.Projects = append(rep.Projects, pr)
		if in.Objects != nil && in.ObjectPrefix != nil {
			o := CleanObjects(ctx, in.Objects, in.ObjectPrefix(ref.ID), pr, opts.DryRun)
			rep.Objects = append(rep.Objects, o)
		}
	}
	return rep, nil
}

// projectInput derives one project's targets from the layout authority.
func projectInput(in HubInput, legacy Tree, ref ProjectRef) (ProjectInput, error) {
	pi := ProjectInput{
		ID:            ref.ID,
		Slug:          ref.Slug,
		HubProjects:   legacy,
		GlobalDir:     in.GlobalDir,
		Tracked:       in.Tracked,
		LocalCopyOnly: ref.LocalBrokerTree,
		Git:           ref.Git,
	}
	pi.WorkspaceOwner = layoutOwner(in.Workspace)
	pi.SharedDirOwner = layoutOwner(in.SharedDirs)
	if !in.Workspace.Backend.Shared() {
		// Precondition: the workspace steps act only
		// when the workspace Layout is shared.
		pi.SkipWorkspace = true
	} else {
		root, err := in.Workspace.ProjectRoot(ref.ID)
		if err != nil {
			return ProjectInput{}, fmt.Errorf("resolve project root: %w", err)
		}
		pi.ProjectRoot = Tree{Base: root.HostBase, Rel: root.RelPath}
		if len(ref.Agents) > 0 {
			layout, agents, recheck, projectID := in.Workspace, ref.Agents, in.AgentStopped, ref.ID
			step := func(opts Options) []Result { return cloudRunHomes(layout, projectID, agents, recheck, opts) }
			pi.CloudRunHomes = step
			if in.ProvisionLock != nil {
				// Step 5 runs under the project's provisioning locks (the
				// workspace's lock directories exist for every project),
				// bounded by the locks' context.
				ws, err := in.Workspace.Resolve(projectfs.Project{ID: ref.ID, Slug: ref.Slug}, "", projectfs.WorkspaceMount(projectfs.ModeSharedPlain, ""))
				if err != nil {
					return ProjectInput{}, fmt.Errorf("resolve workspace: %w", err)
				}
				lock, wsTree := in.ProvisionLock, Tree{Base: ws.HostBase, Rel: ws.RelPath}
				pi.CloudRunHomes = func(opts Options) []Result {
					// A dry run never locks or writes, and a project with no
					// legacy Cloud Run home has nothing to move: neither takes
					// the provisioning locks (which create the state dir).
					if opts.DryRun || !cloudRunHomesPresent(layout, projectID, agents) {
						return step(opts)
					}
					var out []Result
					err := lock(wsTree, func(lockCtx context.Context) error {
						o := opts
						o.Context = lockCtx
						out = step(o)
						return nil
					})
					if err != nil {
						out = append(out, Result{Step: StepCloudRunHomes, Source: wsTree.String(), Outcome: OutcomeFailed, Err: fmt.Errorf("provisioning lock: %w", err)})
					}
					return out
				}
			}
		}
		if in.ProvisionLock != nil && !ref.NoWorkspace {
			ws, err := in.Workspace.Resolve(projectfs.Project{ID: ref.ID, Slug: ref.Slug}, "", projectfs.WorkspaceMount(projectfs.ModeSharedPlain, ""))
			if err != nil {
				return ProjectInput{}, fmt.Errorf("resolve workspace: %w", err)
			}
			lock, wsTree := in.ProvisionLock, Tree{Base: ws.HostBase, Rel: ws.RelPath}
			pi.LockWorkspace = func(section func(lockCtx context.Context) error) error {
				return lock(wsTree, section)
			}
		}
	}
	if !ref.NoWorkspace && !pi.SkipWorkspace {
		// The project-scoped workspace Mount, as the hub's Files tab
		// resolves it (projectWorkspaceResolved).
		ws, err := in.Workspace.Resolve(projectfs.Project{ID: ref.ID, Slug: ref.Slug}, "", projectfs.WorkspaceMount(projectfs.ModeSharedPlain, ""))
		if err != nil {
			return ProjectInput{}, fmt.Errorf("resolve workspace: %w", err)
		}
		pi.Workspace = &Tree{Base: ws.HostBase, Rel: ws.RelPath}
		pi.PrepareWorkspace = func() error { return ensureLeaf(ws) }
	}
	if in.SharedDirs.Backend.Shared() {
		layout := in.SharedDirs
		pi.SharedDir = func(name string) (SharedDirTarget, error) {
			r, err := layout.Resolve(projectfs.Project{ID: ref.ID, Slug: ref.Slug}, "", projectfs.SharedDirMount(api.SharedDir{Name: name}, ""))
			if err != nil {
				return SharedDirTarget{}, err
			}
			return SharedDirTarget{
				Tree:    Tree{Base: r.HostBase, Rel: r.RelPath},
				Prepare: func() error { return ensureLeaf(r) },
			}, nil
		}
	}
	return pi, nil
}

// WriteLog writes the human-readable report: one line per step that did
// (or would do) something or failed, its conflicts and refusals, notes,
// object cleanups, and a summary line.
func (h HubReport) WriteLog(w io.Writer) {
	pf := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }
	if h.DryRun {
		pf("dry run: nothing is changed")
	}
	if h.Skipped != "" {
		pf("skipped: %s", h.Skipped)
	}
	var conflicts, refused, failed int
	for i, p := range h.Projects {
		for _, r := range flatten(p.Results) {
			conflicts += len(r.Conflicts)
			refused += len(r.Refused)
			if r.Outcome == OutcomeNothing && r.Err == nil && len(r.Conflicts) == 0 && len(r.Refused) == 0 {
				continue
			}
			line := fmt.Sprintf("project %s (%s): step %s %s %s -> %s", p.ID, p.Slug, r.Step, r.Outcome, r.Source, r.Target)
			if r.Copied > 0 || r.Identical > 0 {
				line += fmt.Sprintf(" copied=%d identical=%d", r.Copied, r.Identical)
			}
			if r.Backup != "" {
				line += " source kept as " + r.Backup
			}
			if r.Err != nil {
				failed++
				line += " error: " + r.Err.Error()
			}
			pf("%s", line)
			for _, c := range r.Conflicts {
				pf("  CONFLICT %s (target kept; the source's version stays in the .migrated copy)", c)
			}
			for _, rf := range r.Refused {
				pf("  REFUSED %s (not copied)", rf)
			}
		}
		if p.Moves() > 0 && !h.DryRun {
			if errs := p.OwnershipErrors(); len(errs) > 0 {
				pf("project %s (%s): migrated; ownership not normalised (%d entries failed, first: %v); %s", p.ID, p.Slug, len(errs), errs[0], OwnershipHint)
			} else if p.OwnershipRequested {
				pf("project %s (%s): ownership normalised", p.ID, p.Slug)
			}
		}
		for _, n := range p.Notes {
			pf("project %s (%s): note: %s", p.ID, p.Slug, n)
		}
		if i < len(h.Objects) {
			o := h.Objects[i]
			if o.Outcome != ObjectsNone {
				line := fmt.Sprintf("project %s (%s): objects %s %s", p.ID, p.Slug, o.Outcome, o.Prefix)
				if o.Err != nil {
					line += " (" + o.Err.Error() + ")"
				}
				pf("%s", line)
			}
		}
	}
	var objDeleted int
	for _, o := range h.Objects {
		if o.Outcome == ObjectsDeleted || o.Outcome == ObjectsWouldDelete {
			objDeleted++
		}
	}
	pf("summary: projects=%d moves=%d conflicts=%d refused=%d failed=%d object-prefixes-deleted=%d dry-run=%t",
		len(h.Projects), h.Moves(), conflicts, refused, failed, objDeleted, h.DryRun)
}

// flatten expands Entries moves into their per-entry results.
func flatten(rs []Result) []Result {
	var out []Result
	for _, r := range rs {
		if len(r.Entries) > 0 {
			for _, e := range r.Entries {
				if e.Step == "" {
					e.Step = r.Step
				}
				out = append(out, e)
			}
			if r.Err != nil && !strings.HasPrefix(r.Err.Error(), "entry ") {
				out = append(out, Result{Step: r.Step, Source: r.Source, Target: r.Target, Outcome: OutcomeFailed, Err: r.Err})
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// layoutOwner is the uid:gid a Layout names, or nil when it names none.
func layoutOwner(l projectfs.Layout) *Ownership {
	if l.UID <= 0 && l.GID <= 0 {
		return nil
	}
	uid, gid := l.UID, l.GID
	if uid <= 0 {
		uid = -1 // fchownat: leave unchanged
	}
	if gid <= 0 {
		gid = -1
	}
	return &Ownership{UID: uid, GID: gid}
}

// ensureLeaf creates r's directory if absent through the authority
// (Resolved.Open(create): shareddirs.EnsureLeaf, mode 2775 and the default
// ACL, the same leaf creation the broker uses).
func ensureLeaf(r projectfs.Resolved) error {
	root, err := r.Open(true)
	if err != nil {
		return err
	}
	return root.Close()
}
