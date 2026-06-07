/*
Copyright 2026 The Scion Authors.
*/
package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

var (
	provisionWorkspace    string
	provisionMode         string
	provisionDepth        int
	provisionUID          int
	provisionGID          int
	provisionWaitSentinel bool
	provisionTimeout      int
	provisionPollInterval int
)

var provisionCmd = &cobra.Command{
	Use:   "provision",
	Short: "Provision an NFS workspace (clone or wait for sentinel)",
	Long: `Provision a shared workspace in an NFS-backed init container.

In default (clone) mode, reads SCION_CLONE_URL and SCION_CLONE_BRANCH from
the environment and invokes the shared provisioning function. The sentinel
file (.scion-provisioned) is placed inside the workspace directory itself
because the init container's PVC subPath mount only exposes the workspace
dir, not its parent.

In --wait-for-sentinel mode, polls for the sentinel file written by the
winning node's init container and exits 0 when found or non-zero on timeout.

URL and branch are ALWAYS read from environment variables (never from flags)
to prevent shell injection via crafted values.`,
	SilenceErrors: true,
	SilenceUsage:  true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if provisionWaitSentinel {
			return runWaitForSentinel(cmd.Context())
		}
		return runProvision(cmd.Context())
	},
}

func init() {
	rootCmd.AddCommand(provisionCmd)

	provisionCmd.Flags().StringVar(&provisionWorkspace, "workspace", "/workspace",
		"Path to the workspace directory")
	provisionCmd.Flags().StringVar(&provisionMode, "mode", "shared-plain",
		"Workspace sharing mode (shared-plain, worktree-per-agent)")
	provisionCmd.Flags().IntVar(&provisionDepth, "depth", 1,
		"Git clone depth (1=shallow, 0=full, -1=no depth flag)")
	provisionCmd.Flags().IntVar(&provisionUID, "uid", 1000,
		"UID for chown of provisioned files")
	provisionCmd.Flags().IntVar(&provisionGID, "gid", 1000,
		"GID for chown of provisioned files")
	provisionCmd.Flags().BoolVar(&provisionWaitSentinel, "wait-for-sentinel", false,
		"Poll for sentinel file instead of provisioning (lock-loser mode)")
	provisionCmd.Flags().IntVar(&provisionTimeout, "timeout", 300,
		"Timeout in seconds for --wait-for-sentinel mode")
	provisionCmd.Flags().IntVar(&provisionPollInterval, "poll-interval", 2,
		"Poll interval in seconds for --wait-for-sentinel mode")
}

func runProvision(ctx context.Context) error {
	cloneURL := os.Getenv("SCION_CLONE_URL")
	cloneBranch := os.Getenv("SCION_CLONE_BRANCH")
	projectID := os.Getenv("SCION_PROJECT_ID")
	if projectID == "" {
		projectID = "unknown"
	}

	var gc *api.GitCloneConfig
	if cloneURL != "" {
		gc = &api.GitCloneConfig{
			URL:    cloneURL,
			Branch: cloneBranch,
			Depth:  provisionDepth,
		}
	}

	mode := store.ResolveWorkspaceSharingMode(provisionMode)

	in := provision.ProvisionInput{
		Ctx: ctx,
		Resolved: provision.ResolvedWorkspace{
			HostPath: provisionWorkspace,
		},
		ProjectID:   projectID,
		Mode:        mode,
		GitClone:    gc,
		Locker:      nil, // no advisory locker in init container
		NFSUID:      provisionUID,
		NFSGID:      provisionGID,
		SentinelDir: provisionWorkspace,
	}

	log.Info("Provisioning workspace at %s (mode=%s, project=%s)", provisionWorkspace, mode, projectID)
	if err := provision.ProvisionShared(in); err != nil {
		return fmt.Errorf("provision failed: %w", err)
	}
	log.Info("Workspace provisioned successfully")
	return nil
}

func runWaitForSentinel(ctx context.Context) error {
	sentinelPath := filepath.Join(provisionWorkspace, provision.ProvisionSentinelFile)
	timeout := time.Duration(provisionTimeout) * time.Second
	interval := time.Duration(provisionPollInterval) * time.Second
	start := time.Now()
	deadline := start.Add(timeout)

	log.Info("Waiting for sentinel %s (timeout=%s, interval=%s)", sentinelPath, timeout, interval)

	for {
		if _, err := os.Stat(sentinelPath); err == nil {
			log.Info("Sentinel found after %s", time.Since(start).Truncate(time.Second))
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for sentinel %s after %s", sentinelPath, timeout)
		}

		// Sleep for the poll interval, but wake immediately on cancellation
		// (SIGTERM/SIGINT) so the init container exits promptly.
		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled while waiting for sentinel %s: %w", sentinelPath, ctx.Err())
		case <-time.After(interval):
		}
	}
}
