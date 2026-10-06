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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	_ "github.com/rclone/rclone/backend/googlecloudstorage"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/sync"
)

// workspaceIdentityExcludes are the rclone exclude rules for a workspace's
// project identity entry, config.DotScion: a marker file (non-git projects)
// or a directory holding the project-id file and project settings (git
// projects). They are applied to every sync in both directions. They are
// anchored to the sync root, so only the root identity entry is excluded: "/.scion" matches it as a file and
// "/.scion/**" as a directory with everything under it. A .scion entry
// further down the tree is synced as ordinary content.
var workspaceIdentityExcludes = []string{
	"/" + config.DotScion,
	"/" + config.DotScion + "/**",
}

// identityFilteredContext returns ctx carrying an rclone filter that
// excludes the workspace identity entry. Project identity is node-local:
// each host writes its own .scion entry, so it never enters or leaves the
// shared bucket. Excluded entries already on the destination are left
// alone (delete_excluded stays off), so a host's own .scion entry survives
// a sync and a stray .scion/ prefix already in the bucket is neither
// downloaded nor deleted. Symlinks are not followed (no copy_links), which
// remains rclone's default.
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

// syncFiltered makes dst match src, excluding the workspace identity entry.
// It is the only place in this package that calls sync.Sync; a test fails
// the build if another call appears.
func syncFiltered(ctx context.Context, dst, src fs.Fs) error {
	fctx, err := identityFilteredContext(ctx)
	if err != nil {
		return err
	}
	if err := sync.Sync(fctx, dst, src, false); err != nil {
		return fmt.Errorf("rclone sync failed: %w", err)
	}
	return nil
}

// SyncToGCS uploads a local directory to a GCS bucket prefix.
// It uses rclone to sync the local path to the GCS destination. The root
// .scion identity entry is never uploaded (see identityFilteredContext).
func SyncToGCS(ctx context.Context, localPath, bucketName, prefix string) error {
	// Initialize rclone config (required for some backends, safe to call multiple times)
	// We rely on on-the-fly backends and ADC, so no specific config file is needed.

	srcFs, err := fs.NewFs(ctx, localPath)
	if err != nil {
		return fmt.Errorf("failed to create source fs for %s: %w", localPath, err)
	}

	gcsPath := fmt.Sprintf(":gcs,bucket_policy_only=true:%s", bucketName)
	if prefix != "" {
		gcsPath = fmt.Sprintf(":gcs,bucket_policy_only=true:%s/%s", bucketName, prefix)
	}

	dstFs, err := fs.NewFs(ctx, gcsPath)
	if err != nil {
		return fmt.Errorf("failed to create destination fs for %s: %w", gcsPath, err)
	}

	fmt.Printf("Syncing %s to %s via rclone\n", localPath, gcsPath)

	return syncFiltered(ctx, dstFs, srcFs)
}

// SyncFromGCS downloads a GCS bucket prefix to a local directory. The root
// .scion identity entry is never downloaded, and the local one is kept (see
// identityFilteredContext).
func SyncFromGCS(ctx context.Context, bucketName, prefix, localPath string) error {
	gcsPath := fmt.Sprintf(":gcs,bucket_policy_only=true:%s", bucketName)
	if prefix != "" {
		gcsPath = fmt.Sprintf(":gcs,bucket_policy_only=true:%s/%s", bucketName, prefix)
	}

	srcFs, err := fs.NewFs(ctx, gcsPath)
	if err != nil {
		return fmt.Errorf("failed to create source fs for %s: %w", gcsPath, err)
	}

	dstFs, err := fs.NewFs(ctx, localPath)
	if err != nil {
		return fmt.Errorf("failed to create destination fs for %s: %w", localPath, err)
	}

	fmt.Printf("Syncing %s to %s via rclone\n", gcsPath, localPath)

	return syncFiltered(ctx, dstFs, srcFs)
}
