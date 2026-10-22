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

package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	_ "github.com/rclone/rclone/backend/googlecloudstorage"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/sync"
)

// workspaceIdentityExcludes are the rclone exclude rules for a directory's
// project identity entry, config.DotScion: a marker file (non-git projects)
// or a directory holding the project-id file and project settings (git
// projects). SyncToGCS and SyncFromGCS, which GCS volumes use, apply them
// in both directions. They are anchored to the sync root, so only the root
// entry is excluded, with everything under it. Workspace transfers use the
// workspace sync rules from pkg/transfer instead (see SyncWorkspaceToGCS).
var workspaceIdentityExcludes = []string{
	"/" + config.DotScion,
	"/" + config.DotScion + "/**",
}

// syncFilter names the rclone filter a sync runs with.
type syncFilter int

const (
	// filterIdentity excludes only the root identity entry (GCS volumes).
	filterIdentity syncFilter = iota
	// filterWorkspaceContent passes only workspace content
	// (transfer.WorkspaceSyncContentRcloneRules): no .git, no .scion at
	// any depth, no .DS_Store.
	filterWorkspaceContent
	// filterWorkspaceGitState passes only the git ref state
	// (transfer.WorkspaceSyncGitStateRcloneRules): HEAD, packed-refs,
	// index, shallow and refs/**, synced as one unit.
	filterWorkspaceGitState
	// filterWorkspaceGitObject passes only git objects
	// (transfer.WorkspaceSyncGitObjectRcloneRules), only ever copied.
	filterWorkspaceGitObject
)

// filteredContext returns ctx carrying the rclone filter f. Excluded
// entries already on the destination are left alone (delete_excluded stays
// off), so a host's own .scion entry and its own .git survive a sync, and
// stray excluded entries already in the bucket are neither downloaded nor
// deleted. Symlinks are not followed (no copy_links), which remains
// rclone's default.
func filteredContext(ctx context.Context, f syncFilter) (context.Context, error) {
	var rules filter.RulesOpt
	switch f {
	case filterIdentity:
		rules.ExcludeRule = workspaceIdentityExcludes
	case filterWorkspaceContent:
		rules.FilterRule = transfer.WorkspaceSyncContentRcloneRules
	case filterWorkspaceGitState:
		rules.FilterRule = transfer.WorkspaceSyncGitStateRcloneRules
	case filterWorkspaceGitObject:
		rules.FilterRule = transfer.WorkspaceSyncGitObjectRcloneRules
	default:
		return nil, fmt.Errorf("unknown workspace sync filter %d", f)
	}
	// Only the rules are set; age and size limits are explicitly off
	// (their zero values are not "off" in rclone). Built from scratch
	// rather than from rclone's global filter options, so nothing set
	// globally can widen or replace these rules.
	fi, err := filter.NewFilter(&filter.Options{
		RulesOpt: rules,
		MinAge:   fs.DurationOff,
		MaxAge:   fs.DurationOff,
		MinSize:  fs.SizeSuffix(-1),
		MaxSize:  fs.SizeSuffix(-1),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build workspace sync filter: %w", err)
	}
	return filter.ReplaceConfig(ctx, fi), nil
}

// syncStatsSeq numbers the rclone stats groups used by syncFiltered.
var syncStatsSeq atomic.Uint64

// withSyncStatsGroup returns ctx with a stats group of its own. rclone
// keeps transfer errors in its stats, and without a group every sync in
// the process shares one global error count that is never reset. Once any
// sync had failed, every later sync then refused to delete extraneous
// files ("not deleting files as there were IO errors") until the process
// restarted. The groups are bounded: rclone keeps at most
// max_stats_groups of them (default 1000) and discards the oldest when a
// new one is added. This assumes a group outlives its sync. If 1000 or
// more syncs started while one was still running, that sync's group could
// be discarded; rclone would then recreate it with a zero error count, and
// a sync that had hit IO errors could still delete files.
func withSyncStatsGroup(ctx context.Context) context.Context {
	return accounting.WithStatsGroup(ctx, fmt.Sprintf("scion-workspace-sync-%d", syncStatsSeq.Add(1)))
}

// syncFiltered makes dst match src under the filter f (identity, content
// or git ref state; never git objects, which are only ever copied). Only
// files the filter passes are deleted from dst. It is the only place in
// this package that calls sync.Sync; a guard test fails if another call
// appears.
func syncFiltered(ctx context.Context, dst, src fs.Fs, f syncFilter) error {
	if f == filterWorkspaceGitObject {
		return fmt.Errorf("git objects are copied, never synced")
	}
	fctx, err := filteredContext(withSyncStatsGroup(ctx), f)
	if err != nil {
		return err
	}
	if err := sync.Sync(fctx, dst, src, false); err != nil {
		return fmt.Errorf("rclone sync failed: %w", err)
	}
	return nil
}

// copyGitObjects copies the workspace git objects from src to dst. It
// never deletes anything on dst. It is the only place in this package that
// calls sync.CopyDir; a guard test fails if another call appears.
func copyGitObjects(ctx context.Context, dst, src fs.Fs) error {
	fctx, err := filteredContext(withSyncStatsGroup(ctx), filterWorkspaceGitObject)
	if err != nil {
		return err
	}
	if err := sync.CopyDir(fctx, dst, src, false); err != nil {
		return fmt.Errorf("rclone git object copy failed: %w", err)
	}
	return nil
}

// syncGitData carries git data from src to dst: the objects are copied
// first (never deleted), then the ref state is synced as one unit, so a
// ref the source packed or deleted is removed from dst too.
func syncGitData(ctx context.Context, dst, src fs.Fs) error {
	if err := copyGitObjects(ctx, dst, src); err != nil {
		return err
	}
	return syncFiltered(ctx, dst, src, filterWorkspaceGitState)
}

// gcsRemote returns the rclone remote for a bucket prefix.
func gcsRemote(bucketName, prefix string) string {
	if prefix != "" {
		return fmt.Sprintf(":gcs,bucket_policy_only=true:%s/%s", bucketName, prefix)
	}
	return fmt.Sprintf(":gcs,bucket_policy_only=true:%s", bucketName)
}

// openPair opens the rclone source and destination. A local side is opened
// as a plain path, with rclone's default of skipping symlinks.
func openPair(ctx context.Context, src, dst string) (fs.Fs, fs.Fs, error) {
	srcFs, err := fs.NewFs(ctx, src)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create source fs for %s: %w", src, err)
	}
	dstFs, err := fs.NewFs(ctx, dst)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create destination fs for %s: %w", dst, err)
	}
	return srcFs, dstFs, nil
}

// syncLocalToRemote makes remote match the local directory localPath,
// excluding only the root identity entry (GCS volumes).
func syncLocalToRemote(ctx context.Context, localPath, remote string) error {
	srcFs, dstFs, err := openPair(ctx, localPath, remote)
	if err != nil {
		return err
	}
	fmt.Printf("Syncing %s to %s via rclone\n", localPath, remote)
	return syncFiltered(ctx, dstFs, srcFs, filterIdentity)
}

// syncRemoteToLocal makes the local directory localPath match remote,
// excluding only the root identity entry (GCS volumes).
func syncRemoteToLocal(ctx context.Context, remote, localPath string) error {
	srcFs, dstFs, err := openPair(ctx, remote, localPath)
	if err != nil {
		return err
	}
	fmt.Printf("Syncing %s to %s via rclone\n", remote, localPath)
	return syncFiltered(ctx, dstFs, srcFs, filterIdentity)
}

// uploadWorkspace makes remote's content match the workspace at localPath
// (filterWorkspaceContent) and carries the workspace's git data next to it
// (syncGitData), unless the repository cannot travel as git data
// (transfer.WorkspaceGitDataUnsupported): its git data is then skipped
// with a warning. Excluded entries already at remote are left alone.
func uploadWorkspace(ctx context.Context, localPath, remote string) error {
	srcFs, dstFs, err := openPair(ctx, localPath, remote)
	if err != nil {
		return err
	}
	fmt.Printf("Syncing workspace %s to %s via rclone\n", localPath, remote)
	if err := syncFiltered(ctx, dstFs, srcFs, filterWorkspaceContent); err != nil {
		return err
	}
	if reason := transfer.WorkspaceGitDataUnsupported(localPath); reason != "" {
		slog.Warn("Workspace upload skipped git data: commits travel only by git push", "reason", reason)
		return nil
	}
	return syncGitData(ctx, dstFs, srcFs)
}

// downloadWorkspace makes the workspace at localPath match remote's
// content, and with gitData also applies remote's git data. It never
// writes into localPath with rclone: the download goes to a private
// staging directory first, and transfer.ApplyStagedWorkspace applies it
// without following symlinks in the workspace (rclone's local backend
// writes through a symlinked directory it finds on the destination). The
// staging directory receives only what the workspace sync rules pass, and
// the apply refuses anything else again, so entries an earlier upload
// left in the bucket (repository config, hooks, project configuration)
// never reach the workspace.
func downloadWorkspace(ctx context.Context, remote, localPath string, gitData bool) error {
	staging, err := os.MkdirTemp("", "scion-workspace-")
	if err != nil {
		return fmt.Errorf("failed to create staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	srcFs, dstFs, err := openPair(ctx, remote, staging)
	if err != nil {
		return err
	}
	fmt.Printf("Syncing %s to workspace %s via rclone\n", remote, localPath)
	if err := syncFiltered(ctx, dstFs, srcFs, filterWorkspaceContent); err != nil {
		return err
	}
	if gitData {
		if err := syncGitData(ctx, dstFs, srcFs); err != nil {
			return err
		}
	}
	// rclone created a missing destination; keep doing so.
	if err := os.MkdirAll(localPath, 0o755); err != nil {
		return fmt.Errorf("failed to create workspace directory: %w", err)
	}
	stats, err := transfer.ApplyStagedWorkspace(staging, localPath, transfer.StagedApplyOptions{GitData: gitData})
	if err != nil {
		return fmt.Errorf("failed to apply workspace download: %w", err)
	}
	if stats.Refused > 0 {
		slog.Warn("Workspace download refused entries that are never synced", "count", stats.Refused)
	}
	if stats.GitDataSkipped {
		slog.Warn("Workspace download skipped git data: the workspace .git is not a directory")
	}
	return nil
}

// SyncToGCS uploads a local directory to a GCS bucket prefix (GCS
// volumes). The root .scion identity entry is never uploaded by it. We rely
// on on-the-fly backends and ADC, so no rclone config file is needed.
func SyncToGCS(ctx context.Context, localPath, bucketName, prefix string) error {
	return syncLocalToRemote(ctx, localPath, gcsRemote(bucketName, prefix))
}

// SyncFromGCS downloads a GCS bucket prefix to a local directory (GCS
// volumes). The root .scion identity entry is never downloaded by it, and
// the local one is kept.
func SyncFromGCS(ctx context.Context, bucketName, prefix, localPath string) error {
	return syncRemoteToLocal(ctx, gcsRemote(bucketName, prefix), localPath)
}

// SyncWorkspaceToGCS uploads a workspace to a GCS bucket prefix: its
// content, and its git data (see transfer.ClassifyWorkspaceSyncPath).
// Repository configuration, hooks and every .scion entry are never
// uploaded.
func SyncWorkspaceToGCS(ctx context.Context, localPath, bucketName, prefix string) error {
	return uploadWorkspace(ctx, localPath, gcsRemote(bucketName, prefix))
}

// SyncWorkspaceFromGCS downloads a workspace upload into a workspace: its
// content, and its git data, which is applied only into a real .git
// directory and never deleted. Used where a whole repository moves between
// hosts: a broker's create-time workspace download and the hub's
// workspace sync-back.
func SyncWorkspaceFromGCS(ctx context.Context, bucketName, prefix, localPath string) error {
	return downloadWorkspace(ctx, gcsRemote(bucketName, prefix), localPath, true)
}

// SyncWorkspaceContentFromGCS downloads only a workspace upload's content
// into a workspace, leaving its .git untouched. Used by a broker's
// workspace apply (sync-to), whose uploads never carry git data.
func SyncWorkspaceContentFromGCS(ctx context.Context, bucketName, prefix, localPath string) error {
	return downloadWorkspace(ctx, gcsRemote(bucketName, prefix), localPath, false)
}
