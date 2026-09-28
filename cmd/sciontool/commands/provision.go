/*
Copyright 2026 The Scion Authors.
*/
package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		"Git clone depth (0=full clone, >0=that depth; default 1=shallow)")
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
		depthVal := provisionDepth
		gc = &api.GitCloneConfig{
			URL:    cloneURL,
			Branch: cloneBranch,
			Depth:  &depthVal,
		}
	}

	mode := store.ResolveWorkspaceSharingMode(provisionMode)

	// F-111 (design §9): the k8s runtime mounts each NFS-backed shared dir
	// into this init container at its own path (mirroring the main
	// container's mounts) and passes "name=mountPath" pairs here, comma-joined
	// — mkdir+chown must reach them the same as the workspace dir, since
	// they're separate volume mounts the workspace's own chown doesn't reach.
	// Keyed explicitly by the shared dir's own name (the producer side,
	// pkg/runtime/k8s_runtime.go's nfsSharedDirMount, carries it alongside
	// the mount for exactly this reason), not derived from the path here —
	// two shared dirs could produce the same path basename through different
	// target shapes (InWorkspace vs not), so reconstructing the key from the
	// path on this side would risk a silent collision.
	sharedDirs := make(map[string]provision.ResolvedSharedDir)
	if raw := os.Getenv("SCION_SHARED_DIR_PATHS"); raw != "" {
		for i, pair := range strings.Split(raw, ",") {
			if pair == "" {
				continue
			}
			name, path, ok := strings.Cut(pair, "=")
			if !ok || name == "" || path == "" {
				log.Info("SCION_SHARED_DIR_PATHS: skipping malformed entry %q (want name=path)", pair)
				continue
			}
			key := name
			if _, dup := sharedDirs[key]; dup {
				key = fmt.Sprintf("%s-%d", name, i)
			}
			sharedDirs[key] = provision.ResolvedSharedDir{HostPath: path}
		}
	}

	in := provision.ProvisionInput{
		Ctx: ctx,
		Resolved: provision.ResolvedWorkspace{
			HostPath:   provisionWorkspace,
			SharedDirs: sharedDirs,
		},
		ProjectID:   projectID,
		Mode:        mode,
		GitClone:    gc,
		Locker:      nil, // no advisory locker in init container
		NFSUID:      provisionUID,
		NFSGID:      provisionGID,
		SentinelDir: provisionWorkspace,
		// F-111: this command's entire purpose is the chown. A silent
		// failure here would reproduce the "workspace stuck root:root" bug
		// invisibly — the sentinel would still get written, and every future
		// pod for this project would see it and skip provisioning forever.
		// Failing the init container (non-zero exit, pod doesn't start) is
		// the correct, loud failure mode.
		RequireChownSuccess: true,
	}

	log.Info("Provisioning workspace at %s (mode=%s, project=%s, shared_dirs=%d)",
		provisionWorkspace, mode, projectID, len(sharedDirs))
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
