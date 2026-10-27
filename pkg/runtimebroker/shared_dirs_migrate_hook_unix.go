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

package runtimebroker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs/migrate"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// Broker part of the project-storage migration's shared-dir step
// (ptone/scion#5823, ptone/scion#5819). Before the broker Layout placed
// shared dirs on the shared tree, a broker kept a hub-managed project's
// shared dirs node-local at ~/.scion/project-configs/<slug>__<id8>/
// shared-dirs/<name>. Agents on this broker now mount
// <ProjectRoot>/shared-dirs/<name> instead, so on start a broker whose
// Layout resolves shared dirs to the shared tree moves that content there
// with the migration's mover (migrate.Move): copy-only into the leaf the
// authority creates, never overwriting a file, symlinks refused, the source
// kept as <name>.migrated-<ts>. Each project's moves run under its
// provisioning locks. Projects are matched by the hub-written identity of
// their configuration on the share (project-id and project-slug), whose
// <slug>__<id8> names the legacy directory. Nothing is deleted; the emptied
// legacy trees go with the project's deletion (ptone/scion#5835). Per-dir
// volumes of other runtimes are not touched.

// migrateLocalSharedDirs runs the hook once (see above) and returns the
// mover's results. It does nothing when the broker's shared-dir Layout is
// not shared or its share is not mounted.
func (s *Server) migrateLocalSharedDirs(ctx context.Context) ([]migrate.Result, error) {
	layout := s.config.StorageLayout.SharedDirLayout()
	if !layout.Backend.Shared() {
		return nil, nil
	}
	if err := s.ensureLayoutShareMounted(ctx, layout); err != nil {
		return nil, fmt.Errorf("shared-dir share not mounted: %w", err)
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return nil, err
	}
	legacyRoot := filepath.Join(globalDir, projectkeys.ProjectConfigsDir)
	entries, err := os.ReadDir(legacyRoot)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	projects, err := sharedTreeProjects(layout)
	if err != nil {
		return nil, err
	}
	var out []migrate.Result
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sdRel := filepath.Join(projectkeys.ProjectConfigsDir, e.Name(), "shared-dirs")
		id, ok := projects[e.Name()]
		if !ok {
			// Not a project on this share, its identity is not seeded yet,
			// or its slug changed: say so when it holds shared dirs, which
			// then stay node-local until a later start matches them.
			if names, err := os.ReadDir(filepath.Join(globalDir, sdRel)); err == nil && len(names) > 0 {
				slog.Info("Node-local shared dirs not migrated: no project on the shared tree matches this project-configs entry",
					"entry", e.Name())
			}
			continue
		}
		names, err := os.ReadDir(filepath.Join(globalDir, sdRel))
		if err != nil {
			continue // no shared dirs kept node-local for this project
		}
		var pending []string
		for _, n := range names {
			if n.IsDir() && !strings.Contains(n.Name(), migrate.MigratedSuffix) {
				pending = append(pending, n.Name())
			}
		}
		if len(pending) == 0 {
			continue
		}
		res, err := s.moveProjectSharedDirs(ctx, layout, id, migrate.Tree{Base: globalDir, Rel: sdRel}, pending)
		if err != nil {
			out = append(out, migrate.Result{Step: migrate.StepSharedDirs, Source: filepath.Join(globalDir, sdRel), Outcome: migrate.OutcomeFailed, Err: err})
			continue
		}
		out = append(out, res...)
	}
	return out, nil
}

// sharedTreeProjects maps the legacy directory name <slug>__<id8> of every
// project on layout's tree whose configuration carries the hub-written
// project-id and project-slug (config.IsSharedProjectConfigDir) to its ID.
func sharedTreeProjects(layout projectfs.Layout) (map[string]string, error) {
	probe, err := layout.ProjectRoot("00000000-0000-0000-0000-000000000000")
	if err != nil {
		return nil, err
	}
	tree := filepath.Dir(probe.HostPath)
	entries, err := os.ReadDir(tree)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		scionDir := filepath.Join(tree, e.Name(), config.DotScion)
		if !config.IsSharedProjectConfigDir(scionDir) {
			continue
		}
		slug, err := config.ReadProjectSlug(scionDir)
		if err != nil {
			continue
		}
		out[projectfs.LocalConfigDirName(slug, e.Name())] = e.Name()
	}
	return out, nil
}

// moveProjectSharedDirs moves the named node-local shared dirs under src
// into project projectID's shared-dir leaves on layout, under the
// project's provisioning locks.
func (s *Server) moveProjectSharedDirs(ctx context.Context, layout projectfs.Layout, projectID string, src migrate.Tree, names []string) ([]migrate.Result, error) {
	ws, err := layout.Resolve(projectfs.Project{ID: projectID}, "", projectfs.WorkspaceMount(projectfs.ModeSharedPlain, ""))
	if err != nil {
		return nil, err
	}
	base, err := filepath.EvalSymlinks(layout.HostBase)
	if err != nil {
		return nil, fmt.Errorf("resolve host base: %w", err)
	}
	stateRel := filepath.Join(filepath.Dir(ws.RelPath), provision.ProvisionStateDirName)
	fd, _, err := shareddirs.EnsureLeaf(base, stateRel)
	if err != nil {
		return nil, fmt.Errorf("create provisioning state dir: %w", err)
	}
	if err := shareddirs.CloseFd(fd); err != nil {
		return nil, err
	}
	// Owners as the hub's migration normalises them: the Layout's uid/gid
	// when it names one (an unset id is left unchanged).
	var owner *migrate.Ownership
	if layout.UID > 0 || layout.GID > 0 {
		owner = &migrate.Ownership{UID: -1, GID: -1}
		if layout.UID > 0 {
			owner.UID = layout.UID
		}
		if layout.GID > 0 {
			owner.GID = layout.GID
		}
	}
	var out []migrate.Result
	err = provision.WithWorkspaceProvisionLocks(ctx, filepath.Join(base, ws.RelPath), func(lockCtx context.Context) error {
		for _, name := range names {
			from := migrate.Tree{Base: src.Base, Rel: filepath.Join(src.Rel, name)}
			r, err := layout.Resolve(projectfs.Project{ID: projectID}, "", projectfs.SharedDirMount(api.SharedDir{Name: name}, ""))
			if err != nil {
				out = append(out, migrate.Result{Step: migrate.StepSharedDirs, Source: from.String(), Outcome: migrate.OutcomeFailed, Err: fmt.Errorf("resolve target: %w", err)})
				continue
			}
			target := r
			out = append(out, migrate.Move(migrate.Spec{
				Step:      migrate.StepSharedDirs,
				Source:    from,
				Target:    migrate.Tree{Base: r.HostBase, Rel: r.RelPath},
				NoRename:  true,
				Normalize: migrate.NormalizeSharedDir,
				PrepareTarget: func() error {
					root, err := target.Open(true)
					if err != nil {
						return err
					}
					return root.Close()
				},
			}, migrate.Options{Context: lockCtx, Owner: owner}))
		}
		return nil
	})
	return out, err
}

// startSharedDirsMigrationHook runs migrateLocalSharedDirs once per process
// in the background (it may wait on the share) and logs its report.
func (s *Server) startSharedDirsMigrationHook(ctx context.Context) {
	if !s.config.StorageLayout.SharedDirLayout().Backend.Shared() {
		return
	}
	s.sharedDirsMigrateOnce.Do(func() {
		go func() {
			res, err := s.migrateLocalSharedDirs(ctx)
			if err != nil {
				slog.Warn("Node-local shared dirs were not migrated to the shared tree; the next broker start retries", "error", err)
				return
			}
			for _, r := range res {
				attrs := []any{"step", r.Step, "outcome", r.Outcome, "source", r.Source, "target", r.Target,
					"copied", r.Copied, "conflicts", len(r.Conflicts), "refused", len(r.Refused)}
				if r.Err != nil {
					slog.Warn("Node-local shared dir migration failed", append(attrs, "error", r.Err)...)
					continue
				}
				if r.Moved() {
					slog.Info("Node-local shared dir migrated to the shared tree", attrs...)
				}
			}
		}()
	})
}
