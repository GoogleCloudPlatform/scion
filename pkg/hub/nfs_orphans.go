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

package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Orphaned agent directory scan on shared NFS project storage
// (ptone/scion#7932, Phase 4 of ptone/scion#7869, gated by
// experiments.RuntimeOrphanReconcile).
//
// Report only: this scanner is strictly read-only (os.ReadDir and os.Lstat
// only; it never follows symlinks, never walks recursively into agent files,
// and never removes or modifies any file or directory).
//
// It inspects <SubPathRoot>/<projectID>/agents/<slug>/ and its immediate
// home-<agentID> and secrets-<agentID> children on each mounted shared
// Layout of the Hub, and records unmatched trees in runtime_orphans with
// kind=nfs_tree and broker_id="hub". Directories belonging to live agents
// (including legacy Cloud Run agents/<agentID>/ directories), agents with an
// in-flight deletion, or agents soft-deleted within the Hub's soft-delete
// purge retention window are not reported.

const (
	// nfsOrphanScanIntervalMinutes is the recurring singleton cadence (24 h).
	nfsOrphanScanIntervalMinutes = 24 * 60

	// nfsOrphanPartialWriteTimeout bounds the detached store write when the
	// caller's scan context expires mid-scan and partial findings are flushed.
	nfsOrphanPartialWriteTimeout = 5 * time.Second

	nfsAgentHomePrefix    = "home-"
	nfsAgentSecretsPrefix = "secrets-"
)

// defaultNFSOrphanMaxDirs bounds how many agent/leaf directories one scan
// inspects before stopping (without resolving absent rows). Var so tests can
// lower it.
var defaultNFSOrphanMaxDirs = 5000

// nfsOrphanVisitHook, when non-nil, is called right after each visited
// directory in scanNFSProjectAgents. Tests only.
var nfsOrphanVisitHook func()

// nfsOrphanScanReport summarizes one execution of scanNFSOrphans.
type nfsOrphanScanReport struct {
	Available         bool
	UnavailableReason string
	DirsVisited       int
	Truncated         bool
	Complete          bool
	OrphansFound      int
	OrphansCreated    int
	LastProjectID     string
	LastSlug          string
}

// nfsOrphanScanHandler returns the 24h recurring singleton handler.
func (s *Server) nfsOrphanScanHandler() func(context.Context) {
	return func(ctx context.Context) {
		if !s.experimentEnabled(experiments.RuntimeOrphanReconcile) {
			return
		}
		report, err := s.scanNFSOrphans(ctx, time.Now(), defaultNFSOrphanMaxDirs)
		if err != nil {
			s.agentLifecycleLog.Warn("nfs orphan scan failed", "error", err,
				"dirs_visited", report.DirsVisited, "orphans_found", report.OrphansFound,
				"last_project_id", report.LastProjectID, "last_slug", report.LastSlug)
		}
		if report.Truncated {
			s.agentLifecycleLog.Warn("nfs orphan scan truncated at directory bound; absent rows were not resolved",
				"max_dirs", defaultNFSOrphanMaxDirs, "dirs_visited", report.DirsVisited,
				"orphans_found", report.OrphansFound, "last_project_id", report.LastProjectID,
				"last_slug", report.LastSlug)
		}
	}
}

// nfsOrphanTarget returns the runtime_target label for tree on sl. When the
// Hub has multiple distinct shared layouts (transitional split), it qualifies
// the tree kind with the layout's share identifier so identical paths on
// separate mounts do not collide on the runtime_orphans unique index.
func nfsOrphanTarget(sl projectfs.Layout, tree projectfs.TreeKind, multiShare bool) string {
	if !multiShare {
		return string(tree)
	}
	prefix := sl.Share.ID
	if prefix == "" {
		prefix = filepath.Base(filepath.Clean(sl.HostBase))
	}
	return prefix + "/" + string(tree)
}

// logNFSOrphanUnavailableOnce logs once per Hub process while shared NFS
// project storage is not mounted/available for scanning.
func (s *Server) logNFSOrphanUnavailableOnce(reason string) {
	if s.nfsOrphanUnavailableLogged.CompareAndSwap(false, true) {
		s.agentLifecycleLog.Info("nfs orphan scan unavailable: hub has no mounted shared project storage", "reason", reason)
	}
}

// scanNFSOrphans performs a bounded, stat-only, read-only scan of mounted
// shared project storage and records kind=nfs_tree rows in runtime_orphans.
// It never removes or modifies any filesystem entry.
func (s *Server) scanNFSOrphans(ctx context.Context, now time.Time, maxDirs int) (nfsOrphanScanReport, error) {
	if maxDirs < 1 {
		maxDirs = defaultNFSOrphanMaxDirs
	}
	layout, err := s.projectStorageLayout()
	if err != nil {
		reason := "project storage layout is unavailable"
		s.logNFSOrphanUnavailableOnce(reason)
		return nfsOrphanScanReport{Available: false, UnavailableReason: reason}, nil
	}
	shared := sharedStorageLayouts(layout)
	if len(shared) == 0 {
		reason := "hub project storage backend is not shared"
		s.logNFSOrphanUnavailableOnce(reason)
		return nfsOrphanScanReport{Available: false, UnavailableReason: reason}, nil
	}

	var mounted []projectfs.Layout
	unmountedCount := 0
	for _, sl := range shared {
		if merr := sl.CheckTreeMounted(); merr != nil {
			unmountedCount++
			continue
		}
		mounted = append(mounted, sl)
	}
	if len(mounted) == 0 {
		reason := "shared project storage is not mounted on the hub"
		s.logNFSOrphanUnavailableOnce(reason)
		return nfsOrphanScanReport{Available: false, UnavailableReason: reason}, nil
	}
	// Shared storage is mounted; allow future unavailability to log once again.
	s.nfsOrphanUnavailableLogged.Store(false)

	multiShare := len(shared) > 1
	retention, _ := s.softDeleteSettings()
	report := nfsOrphanScanReport{
		Available: true,
		Complete:  unmountedCount == 0,
	}

	var orphans []store.RuntimeOrphan
	var scanErr error
	for _, sl := range mounted {
		if err := ctx.Err(); err != nil {
			report.Complete = false
			scanErr = err
			break
		}
		subRoot := sl.SubPathRoot
		if subRoot == "" {
			subRoot = projectfs.DefaultSubPathRoot
		}
		projectsRoot := filepath.Join(sl.HostBase, filepath.FromSlash(subRoot))
		projEntries, err := os.ReadDir(projectsRoot)
		if err != nil {
			report.Complete = false
			s.agentLifecycleLog.Warn("nfs orphan scan: failed to read projects root", "error", err)
			continue
		}
		for _, pe := range projEntries {
			if err := ctx.Err(); err != nil {
				report.Complete = false
				scanErr = err
				break
			}
			// Skip non-directories, symlinks, and entries that are not valid
			// project IDs (matching projectfs.Layout.ProjectTreeIDs).
			if pe.Type()&os.ModeSymlink != 0 || !pe.IsDir() {
				continue
			}
			projectID := pe.Name()
			if !shareddirs.ValidProjectID(projectID) {
				continue
			}
			agentsDir := filepath.Join(projectsRoot, projectID, "agents")
			fi, err := os.Lstat(agentsDir)
			if err != nil {
				if !os.IsNotExist(err) {
					report.Complete = false
				}
				continue
			}
			if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
				continue
			}

			projOrphans, ok, truncated := s.scanNFSProjectAgents(ctx, sl, multiShare, projectID, agentsDir, now, retention, maxDirs, &report)
			orphans = append(orphans, projOrphans...)
			if !ok {
				report.Complete = false
			}
			if err := ctx.Err(); err != nil {
				report.Complete = false
				scanErr = err
				break
			}
			if truncated {
				report.Truncated = true
				report.Complete = false
				break
			}
		}
		if report.Truncated || scanErr != nil {
			break
		}
	}

	report.OrphansFound = len(orphans)
	// If the caller's context expired mid-scan, still flush partial findings
	// with complete=false (never resolving absent rows) using a short detached
	// context so findings collected before the deadline are not lost.
	if scanErr != nil && len(orphans) == 0 {
		return report, scanErr
	}
	writeCtx := ctx
	var cancel context.CancelFunc
	if scanErr != nil {
		writeCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), nfsOrphanPartialWriteTimeout)
		defer cancel()
	}
	created, writeErr := s.store.RecordNFSOrphans(writeCtx, orphans, now, report.Complete)
	for _, o := range created {
		s.agentLifecycleLog.Warn("runtime orphan: nfs agent directory has no hub agent",
			"kind", o.Kind, "reason", o.Reason, "runtime_target", o.RuntimeTarget,
			"project_id", o.ProjectID, "slug", o.Slug, "agent_id", o.AgentID)
	}
	report.OrphansCreated = len(created)
	return report, errors.Join(scanErr, writeErr)
}

// scanNFSProjectAgents scans one project's agents/ directory (stat-only, no
// recursion below agents/<slug>/<child>). It returns the discovered orphans,
// ok=false if a store or filesystem read error prevented a complete view of
// this project, and truncated=true if maxDirs was reached.
func (s *Server) scanNFSProjectAgents(
	ctx context.Context,
	sl projectfs.Layout,
	multiShare bool,
	projectID string,
	agentsDir string,
	now time.Time,
	retention time.Duration,
	maxDirs int,
	report *nfsOrphanScanReport,
) (orphans []store.RuntimeOrphan, ok bool, truncated bool) {
	projectKnown := true
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			projectKnown = false
		} else {
			s.agentLifecycleLog.Warn("nfs orphan scan: failed to look up project", "project_id", projectID, "error", err)
			return nil, false, false
		}
	}

	protectedBySlug := map[string]bool{}
	protectedByID := map[string]bool{}
	allByID := map[string]*store.Agent{}
	deletedBySlug := map[string]bool{}

	if projectKnown {
		agents, err := s.listAllProjectAgentsIncludingDeleted(ctx, projectID)
		if err != nil {
			s.agentLifecycleLog.Warn("nfs orphan scan: failed to list project agents", "project_id", projectID, "error", err)
			return nil, false, false
		}
		for i := range agents {
			a := &agents[i]
			allByID[a.ID] = a
			if agentProtectsNFSTree(a, now, retention) {
				protectedByID[a.ID] = true
				if a.Slug != "" {
					protectedBySlug[a.Slug] = true
				}
			} else if !a.DeletedAt.IsZero() && a.Slug != "" {
				deletedBySlug[a.Slug] = true
			}
		}
	}

	slugEntries, err := os.ReadDir(agentsDir)
	if err != nil {
		s.agentLifecycleLog.Warn("nfs orphan scan: failed to read project agents directory", "project_id", projectID, "error", err)
		return nil, false, false
	}

	ok = true
	for _, se := range slugEntries {
		if ctx.Err() != nil {
			return orphans, false, false
		}
		if se.Type()&os.ModeSymlink != 0 || !se.IsDir() {
			continue
		}
		if report.DirsVisited >= maxDirs {
			return orphans, false, true
		}
		report.DirsVisited++
		slug := se.Name()
		report.LastProjectID = projectID
		report.LastSlug = slug

		if !projectKnown {
			orphans = append(orphans, store.RuntimeOrphan{
				Kind:          store.RuntimeOrphanKindNFSTree,
				BrokerID:      store.RuntimeOrphanBrokerHub,
				RuntimeTarget: nfsOrphanTarget(sl, projectfs.TreeAgentRoot, multiShare),
				ProjectID:     projectID,
				Slug:          slug,
				Reason:        orphanUnknownProject,
			})
		} else if reason := classifyNFSAgentRoot(slug, protectedBySlug, protectedByID, allByID, deletedBySlug); reason != "" {
			orphans = append(orphans, store.RuntimeOrphan{
				Kind:          store.RuntimeOrphanKindNFSTree,
				BrokerID:      store.RuntimeOrphanBrokerHub,
				RuntimeTarget: nfsOrphanTarget(sl, projectfs.TreeAgentRoot, multiShare),
				ProjectID:     projectID,
				Slug:          slug,
				Reason:        reason,
			})
		}
		if nfsOrphanVisitHook != nil {
			nfsOrphanVisitHook()
		}

		// Inspect immediate children of agents/<slug>/ only (never recurse).
		slugDir := filepath.Join(agentsDir, slug)
		leafEntries, err := os.ReadDir(slugDir)
		if err != nil {
			ok = false
			continue
		}
		for _, le := range leafEntries {
			if ctx.Err() != nil {
				return orphans, false, false
			}
			if le.Type()&os.ModeSymlink != 0 || !le.IsDir() {
				continue
			}
			tree, agentID, matched := parseNFSAgentLeaf(le.Name())
			if !matched {
				continue
			}
			if report.DirsVisited >= maxDirs {
				return orphans, false, true
			}
			report.DirsVisited++

			reason := classifyNFSAgentLeaf(projectKnown, slug, agentID, protectedBySlug, protectedByID, allByID)
			if reason != "" {
				orphans = append(orphans, store.RuntimeOrphan{
					Kind:          store.RuntimeOrphanKindNFSTree,
					BrokerID:      store.RuntimeOrphanBrokerHub,
					RuntimeTarget: nfsOrphanTarget(sl, tree, multiShare),
					ProjectID:     projectID,
					Slug:          slug,
					AgentID:       agentID,
					Reason:        reason,
				})
			}
			if nfsOrphanVisitHook != nil {
				nfsOrphanVisitHook()
			}
		}
	}
	return orphans, ok, false
}

// classifyNFSAgentRoot returns the orphan reason for an agents/<name>/
// directory in a known project, or "" when a protected agent owns it.
// Legacy Cloud Run directories named by agent UUID (agents/<agentID>/) are
// recognized via allByID/protectedByID so un-migrated live agents (and agents
// soft-deleted within retention) are never reported as orphans.
func classifyNFSAgentRoot(
	name string,
	protectedBySlug map[string]bool,
	protectedByID map[string]bool,
	allByID map[string]*store.Agent,
	deletedBySlug map[string]bool,
) string {
	if protectedByID[name] || protectedBySlug[name] {
		return ""
	}
	if a := allByID[name]; a != nil && !a.DeletedAt.IsZero() {
		return orphanDeleted
	}
	if deletedBySlug[name] {
		return orphanDeleted
	}
	return orphanNoAgent
}

// agentProtectsNFSTree reports whether agent a owns its NFS directories:
// live agents, agents with an in-flight deletion, and agents soft-deleted
// within the Hub's soft-delete purge retention window all protect their trees.
func agentProtectsNFSTree(a *store.Agent, now time.Time, retention time.Duration) bool {
	if a.DeletedAt.IsZero() {
		return true
	}
	if a.DeletionHoldsRow(now) {
		return true
	}
	if retention > 0 && now.Sub(a.DeletedAt) < retention {
		return true
	}
	return false
}

// parseNFSAgentLeaf matches home-<agentID> and secrets-<agentID> directory
// names where <agentID> is a canonical Hub agent UUID.
func parseNFSAgentLeaf(name string) (projectfs.TreeKind, string, bool) {
	if id, ok := strings.CutPrefix(name, nfsAgentHomePrefix); ok && projectfs.ValidAgentID(id) {
		return projectfs.TreeAgentHome, id, true
	}
	if id, ok := strings.CutPrefix(name, nfsAgentSecretsPrefix); ok && projectfs.ValidAgentID(id) {
		return projectfs.TreeAgentSecrets, id, true
	}
	return "", "", false
}

// classifyNFSAgentLeaf returns the orphan reason for a home-<agentID> or
// secrets-<agentID> child directory, or "" when a protected agent owns it.
func classifyNFSAgentLeaf(
	projectKnown bool,
	slug string,
	agentID string,
	protectedBySlug map[string]bool,
	protectedByID map[string]bool,
	allByID map[string]*store.Agent,
) string {
	if !projectKnown {
		return orphanUnknownProject
	}
	if protectedByID[agentID] {
		return ""
	}
	if a := allByID[agentID]; a != nil && !a.DeletedAt.IsZero() {
		return orphanDeleted
	}
	if protectedBySlug[slug] {
		return orphanSuperseded
	}
	return orphanNoAgent
}

// listAllProjectAgentsIncludingDeleted pages through every agent row (live and
// soft-deleted) of projectID.
func (s *Server) listAllProjectAgentsIncludingDeleted(ctx context.Context, projectID string) ([]store.Agent, error) {
	var all []store.Agent
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res, err := s.store.ListAgents(ctx,
			store.AgentFilter{ProjectID: projectID, IncludeDeleted: true},
			store.ListOptions{Limit: 500, SkipTotalCount: true, Cursor: cursor},
		)
		if err != nil {
			return nil, err
		}
		all = append(all, res.Items...)
		if res.NextCursor == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
}

// NFSOrphanScanExecutor runs the report-only NFS orphan directory scan as an
// on-demand admin maintenance operation (key entadapter.NFSOrphanScanKey).
// It takes no parameters and never deletes or modifies any file or directory.
type NFSOrphanScanExecutor struct {
	srv     *Server
	maxDirs int // optional override for tests
}

// Run implements MaintenanceExecutor.
func (e *NFSOrphanScanExecutor) Run(ctx context.Context, logger io.Writer, _ map[string]string) error {
	maxDirs := e.maxDirs
	if maxDirs < 1 {
		maxDirs = defaultNFSOrphanMaxDirs
	}
	report, err := e.srv.scanNFSOrphans(ctx, time.Now(), maxDirs)
	if !report.Available {
		_, _ = fmt.Fprintf(logger, "NFS orphan scan unavailable: %s\n", report.UnavailableReason)
		return nil
	}
	_, _ = fmt.Fprintf(logger, "Scanned %d directories (complete=%t, truncated=%t): found %d orphan trees (%d newly recorded)\n",
		report.DirsVisited, report.Complete, report.Truncated, report.OrphansFound, report.OrphansCreated)
	if err != nil {
		return fmt.Errorf("nfs orphan scan: %w", err)
	}
	return nil
}
