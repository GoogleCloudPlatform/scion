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

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/sync"
	_ "github.com/rclone/rclone/backend/googlecloudstorage"
	_ "github.com/rclone/rclone/backend/local"
)

// SyncToGCS uploads a local directory to a GCS bucket prefix.
// It uses rclone to sync the local path to the GCS destination.
func SyncToGCS(ctx context.Context, localPath, bucketName, prefix string) error {
	// Initialize rclone config (required for some backends, safe to call multiple times)
	// We rely on on-the-fly backends and ADC, so no specific config file is needed.

	srcFs, err := fs.NewFs(ctx, localPath)
	if err != nil {
		return fmt.Errorf("failed to create source fs for %s: %w", localPath, err)
	}

	gcsPath := fmt.Sprintf(":gcs:%s", bucketName)
	if prefix != "" {
		gcsPath = fmt.Sprintf(":gcs:%s/%s", bucketName, prefix)
	}

	dstFs, err := fs.NewFs(ctx, gcsPath)
	if err != nil {
		return fmt.Errorf("failed to create destination fs for %s: %w", gcsPath, err)
	}

	fmt.Printf("Syncing %s to %s via rclone\n", localPath, gcsPath)

	// sync.Sync requires a context, dest, src, and a bool for 'createEmptySrc' which is mostly for move/copy operations context
	// actually checking signature: Sync(ctx context.Context, dst, src fs.Fs, createEmptySrcDirectories bool) error
	if err := sync.Sync(ctx, dstFs, srcFs, false); err != nil {
		return fmt.Errorf("rclone sync failed: %w", err)
	}

	return nil
}

// SyncFromGCS downloads a GCS bucket prefix to a local directory.
func SyncFromGCS(ctx context.Context, bucketName, prefix, localPath string) error {
	gcsPath := fmt.Sprintf(":gcs:%s", bucketName)
	if prefix != "" {
		gcsPath = fmt.Sprintf(":gcs:%s/%s", bucketName, prefix)
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

	if err := sync.Sync(ctx, dstFs, srcFs, false); err != nil {
		return fmt.Errorf("rclone sync failed: %w", err)
	}

	return nil
}
