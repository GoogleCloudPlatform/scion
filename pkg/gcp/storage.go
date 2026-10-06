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
	"sync/atomic"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	_ "github.com/rclone/rclone/backend/googlecloudstorage"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/sync"
)

// workspaceIdentityExcludes are the rclone exclude rules for a workspace's
// project identity entry, config.DotScion: a marker file (non-git projects)
// or a directory holding the project-id file and project settings (git
// projects). syncFiltered applies them in both directions. They are
// anchored to the sync root, so only the root entry is excluded, with
// everything under it: "/.scion" matches it as a file and "/.scion/**" as
// a directory. A .scion entry further down the tree is synced as ordinary
// content.
var workspaceIdentityExcludes = []string{
	"/" + config.DotScion,
	"/" + config.DotScion + "/**",
}

// identityFilteredContext returns ctx carrying an rclone filter that
// excludes the workspace identity entry. Project identity is node-local:
// each host writes its own .scion entry, so SyncToGCS and SyncFromGCS
// never carry it into or out of the bucket. (This covers only these rclone
// syncs; transfers that do not go through them, such as signed-URL
// uploads, are not filtered here.) Excluded entries already on the
// destination are left alone (delete_excluded stays off), so a host's own
// .scion entry survives a sync and a stray .scion/ prefix already in the
// bucket is neither downloaded nor deleted. Symlinks are not followed (no
// copy_links), which remains rclone's default.
func identityFilteredContext(ctx context.Context) (context.Context, error) {
	// Only the exclude rules are set; age and size limits are explicitly
	// off (their zero values are not "off" in rclone). Built from scratch
	// rather than from rclone's global filter options, so nothing set
	// globally can widen or replace these rules.
	fi, err := filter.NewFilter(&filter.Options{
		RulesOpt: filter.RulesOpt{ExcludeRule: workspaceIdentityExcludes},
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
// restarted. rclone discards the oldest groups once it holds
// max_stats_groups of them, so the groups do not accumulate.
func withSyncStatsGroup(ctx context.Context) context.Context {
	return accounting.WithStatsGroup(ctx, fmt.Sprintf("scion-workspace-sync-%d", syncStatsSeq.Add(1)))
}

// syncFiltered makes dst match src, excluding the workspace identity entry.
// It is the only place in this package that calls sync.Sync; a guard test
// fails if another call appears.
func syncFiltered(ctx context.Context, dst, src fs.Fs) error {
	fctx, err := identityFilteredContext(withSyncStatsGroup(ctx))
	if err != nil {
		return err
	}
	if err := sync.Sync(fctx, dst, src, false); err != nil {
		return fmt.Errorf("rclone sync failed: %w", err)
	}
	return nil
}

// gcsRemote returns the rclone remote for a bucket prefix.
func gcsRemote(bucketName, prefix string) string {
	if prefix != "" {
		return fmt.Sprintf(":gcs,bucket_policy_only=true:%s/%s", bucketName, prefix)
	}
	return fmt.Sprintf(":gcs,bucket_policy_only=true:%s", bucketName)
}

// syncLocalToRemote makes remote match the local directory localPath. The
// local side is opened as a plain path, with rclone's default of skipping
// symlinks.
func syncLocalToRemote(ctx context.Context, localPath, remote string) error {
	srcFs, err := fs.NewFs(ctx, localPath)
	if err != nil {
		return fmt.Errorf("failed to create source fs for %s: %w", localPath, err)
	}

	dstFs, err := fs.NewFs(ctx, remote)
	if err != nil {
		return fmt.Errorf("failed to create destination fs for %s: %w", remote, err)
	}

	fmt.Printf("Syncing %s to %s via rclone\n", localPath, remote)

	return syncFiltered(ctx, dstFs, srcFs)
}

// syncRemoteToLocal makes the local directory localPath match remote.
func syncRemoteToLocal(ctx context.Context, remote, localPath string) error {
	srcFs, err := fs.NewFs(ctx, remote)
	if err != nil {
		return fmt.Errorf("failed to create source fs for %s: %w", remote, err)
	}

	dstFs, err := fs.NewFs(ctx, localPath)
	if err != nil {
		return fmt.Errorf("failed to create destination fs for %s: %w", localPath, err)
	}

	fmt.Printf("Syncing %s to %s via rclone\n", remote, localPath)

	return syncFiltered(ctx, dstFs, srcFs)
}

// SyncToGCS uploads a local directory to a GCS bucket prefix.
// It uses rclone to sync the local path to the GCS destination. The root
// .scion identity entry is never uploaded by it (see
// identityFilteredContext). We rely on on-the-fly backends and ADC, so no
// rclone config file is needed.
func SyncToGCS(ctx context.Context, localPath, bucketName, prefix string) error {
	return syncLocalToRemote(ctx, localPath, gcsRemote(bucketName, prefix))
}

// SyncFromGCS downloads a GCS bucket prefix to a local directory. The root
// .scion identity entry is never downloaded by it, and the local one is
// kept (see identityFilteredContext).
func SyncFromGCS(ctx context.Context, bucketName, prefix, localPath string) error {
	return syncRemoteToLocal(ctx, gcsRemote(bucketName, prefix), localPath)
}
