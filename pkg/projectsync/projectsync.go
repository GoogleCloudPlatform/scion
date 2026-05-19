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

// Package projectsync implements project-level workspace synchronization using
// rclone's WebDAV backend against the Hub's WebDAV endpoint.
package projectsync

import (
	"context"
	"fmt"
	"strings"

	_ "github.com/rclone/rclone/backend/local"
	_ "github.com/rclone/rclone/backend/webdav"
	"github.com/rclone/rclone/cmd/bisync"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/sync"
)

// Direction represents the sync direction for project-level sync.
type Direction string

const (
	// DirPush syncs local project workspace → hub (local is source of truth).
	DirPush Direction = "push"
	// DirPull syncs hub → local project workspace (hub is source of truth).
	DirPull Direction = "pull"
	// DirBisync performs bidirectional sync (newer file wins).
	DirBisync Direction = "bisync"
)

// Options configures a project sync operation.
type Options struct {
	// LocalPath is the local project workspace directory.
	LocalPath string
	// HubEndpoint is the base Hub API URL (e.g. "https://hub.example.com").
	HubEndpoint string
	// ProjectID is the grove identifier on the hub.
	ProjectID string
	// AuthToken is the bearer token for hub authentication.
	AuthToken string
	// Direction specifies the sync direction.
	Direction Direction
	// DryRun previews changes without making them.
	DryRun bool
	// ExcludePatterns are additional glob patterns to exclude from sync.
	ExcludePatterns []string
	// Force bypasses the max-delete safety check (bisync only).
	Force bool
}

// DefaultExcludePatterns are always excluded from project sync.
// These match the patterns used by the hub's WebDAV endpoint.
var DefaultExcludePatterns = []string{
	".git/**",
	".scion/**",
	"node_modules/**",
	"*.env",
}

// Result contains the outcome of a sync operation.
type Result struct {
	// Direction is the sync direction that was performed.
	Direction Direction
	// DryRun indicates whether this was a preview-only run.
	DryRun bool
}

// Sync performs a project workspace sync operation.
func Sync(ctx context.Context, opts Options) (*Result, error) {
	if opts.LocalPath == "" {
		return nil, fmt.Errorf("local workspace path is required")
	}
	if opts.HubEndpoint == "" {
		return nil, fmt.Errorf("hub endpoint is required")
	}
	if opts.ProjectID == "" {
		return nil, fmt.Errorf("project ID is required")
	}

	// Build the WebDAV remote URL
	davURL := buildWebDAVURL(opts.HubEndpoint, opts.ProjectID)

	// Build rclone on-the-fly remote string.
	// Values must be single-quoted so that special characters in the URL
	// (e.g. "://" in https://) and token are not misinterpreted as
	// rclone connection-string delimiters.
	remote := fmt.Sprintf(":webdav,url='%s',bearer_token='%s':", davURL, opts.AuthToken)

	// Set up rclone context with config
	ctx, ci := fs.AddConfig(ctx)
	ci.DryRun = opts.DryRun
	ci.LogLevel = fs.LogLevelNotice
	ci.CheckSum = true // WebDAV doesn't support modtime; use checksums instead

	// Set up file exclusion filters
	ctx, fi := filter.AddConfig(ctx)
	for _, pattern := range DefaultExcludePatterns {
		if err := fi.Add(false, pattern); err != nil {
			return nil, fmt.Errorf("failed to add default exclude pattern %q: %w", pattern, err)
		}
	}
	for _, pattern := range opts.ExcludePatterns {
		if err := fi.Add(false, pattern); err != nil {
			return nil, fmt.Errorf("failed to add exclude pattern %q: %w", pattern, err)
		}
	}

	// Create filesystems
	localFs, err := fs.NewFs(ctx, opts.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create local filesystem: %w", err)
	}

	remoteFs, err := fs.NewFs(ctx, remote)
	if err != nil {
		return nil, fmt.Errorf("failed to create WebDAV remote: %w", err)
	}

	// Perform the sync based on direction
	switch opts.Direction {
	case DirPush:
		// Local → Hub (local is source of truth)
		if err := sync.Sync(ctx, remoteFs, localFs, false); err != nil {
			return nil, fmt.Errorf("push sync failed: %w", err)
		}
	case DirPull:
		// Hub → Local (hub is source of truth)
		if err := sync.Sync(ctx, localFs, remoteFs, false); err != nil {
			return nil, fmt.Errorf("pull sync failed: %w", err)
		}
	case DirBisync:
		bisyncOpts := &bisync.Options{
			Resync: true, // First run always needs resync
			Force:  opts.Force,
			DryRun: opts.DryRun,
		}
		if err := bisync.Bisync(ctx, localFs, remoteFs, bisyncOpts); err != nil {
			return nil, fmt.Errorf("bisync failed: %w", err)
		}
	default:
		return nil, fmt.Errorf("unknown sync direction: %s", opts.Direction)
	}

	return &Result{
		Direction: opts.Direction,
		DryRun:    opts.DryRun,
	}, nil
}

// buildWebDAVURL constructs the WebDAV endpoint URL for a project.
func buildWebDAVURL(hubEndpoint, projectID string) string {
	base := strings.TrimRight(hubEndpoint, "/")
	return fmt.Sprintf("%s/api/v1/projects/%s/dav", base, projectID)
}
