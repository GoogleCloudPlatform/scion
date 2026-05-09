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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// MaintenanceExecutor defines the interface for a runnable maintenance operation.
type MaintenanceExecutor interface {
	// Run executes the operation. The context is cancelled if the server shuts down.
	// The logger captures output that is stored in the run/operation's log field.
	// Params contains operation-specific configuration from the API request.
	Run(ctx context.Context, logger io.Writer, params map[string]string) error
}

// SecretMigrationExecutor migrates hub-scoped secrets from the legacy fixed "hub" scope ID
// to the per-instance hub ID namespace in GCP Secret Manager.
type SecretMigrationExecutor struct {
	store         store.Store
	secretBackend secret.SecretBackend
}

// SecretMigrationResult holds the outcome of a secret migration run.
type SecretMigrationResult struct {
	Migrated int  `json:"migrated"`
	Skipped  int  `json:"skipped"`
	DryRun   bool `json:"dryRun"`
}

func (e *SecretMigrationExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	dryRun := params["dryRun"] == "true"

	// Ensure the secret backend is a GCP SM backend.
	gcpBackend, ok := e.secretBackend.(*secret.GCPBackend)
	if !ok {
		return fmt.Errorf("secret migration requires GCP Secret Manager backend; current backend is not GCP SM")
	}

	// List all secrets from the database (no scope filter = all secrets).
	allSecrets, err := e.store.ListSecrets(ctx, store.SecretFilter{})
	if err != nil {
		return fmt.Errorf("failed to list secrets: %w", err)
	}

	if len(allSecrets) == 0 {
		fmt.Fprintln(logger, "No secrets found to migrate.")
		return nil
	}

	fmt.Fprintf(logger, "Found %d secret(s) to process.\n", len(allSecrets))
	if dryRun {
		fmt.Fprintln(logger, "DRY RUN: No changes will be made.")
	}

	migrated := 0
	skipped := 0

	for _, s := range allSecrets {
		// Skip secrets that already have a GCP SM reference.
		if s.SecretRef != "" {
			fmt.Fprintf(logger, "  SKIP  %s (scope: %s/%s) - already has ref: %s\n", s.Key, s.Scope, s.ScopeID, s.SecretRef)
			skipped++
			continue
		}

		if dryRun {
			fmt.Fprintf(logger, "  WOULD MIGRATE  %s (scope: %s/%s, type: %s)\n", s.Key, s.Scope, s.ScopeID, s.SecretType)
			migrated++
			continue
		}

		// Read value from the database.
		value, err := e.store.GetSecretValue(ctx, s.Key, s.Scope, s.ScopeID)
		if err != nil {
			fmt.Fprintf(logger, "  WARN  %s (scope: %s/%s) - failed to get value: %v\n", s.Key, s.Scope, s.ScopeID, err)
			skipped++
			continue
		}

		// Force-migrate: read the value from the existing GCP SM reference if present.
		// (This path is only reached for secrets without a ref, so no force logic needed here —
		// the CLI --force flag handles re-migration of already-migrated secrets.)

		input := &secret.SetSecretInput{
			Name:        s.Key,
			Value:       value,
			SecretType:  s.SecretType,
			Target:      s.Target,
			Scope:       s.Scope,
			ScopeID:     s.ScopeID,
			Description: s.Description,
			CreatedBy:   s.CreatedBy,
			UpdatedBy:   s.UpdatedBy,
		}

		if _, _, err := gcpBackend.Set(ctx, input); err != nil {
			fmt.Fprintf(logger, "  ERROR  %s (scope: %s/%s) - %v\n", s.Key, s.Scope, s.ScopeID, err)
			skipped++
			continue
		}

		fmt.Fprintf(logger, "  MIGRATED  %s (scope: %s/%s, type: %s)\n", s.Key, s.Scope, s.ScopeID, s.SecretType)
		migrated++
	}

	status := "complete"
	if dryRun {
		status = "dry run complete"
	}
	fmt.Fprintf(logger, "\nMigration %s: %d migrated, %d skipped\n", status, migrated, skipped)

	return nil
}

// ResultJSON returns the migration result as a JSON string.
func (r *SecretMigrationResult) ResultJSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// PullImagesExecutor pulls container images for configured harnesses.
type PullImagesExecutor struct {
	runtimeBin string   // "docker", "podman", or "container"
	registry   string   // image registry prefix
	tag        string   // image tag (default "latest")
	harnesses  []string // harness names (e.g., "claude", "gemini")
}

func (e *PullImagesExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	log := logging.Subsystem("hub.maintenance.pull-images")

	registry := e.registry
	if v := params["registry"]; v != "" {
		registry = v
	}
	tag := e.tag
	if tag == "" {
		tag = "latest"
	}
	if v := params["tag"]; v != "" {
		tag = v
	}

	if registry == "" {
		return fmt.Errorf("no image registry configured; set runtime.image_registry in settings.yaml")
	}

	runtimeBin := e.runtimeBin
	if runtimeBin == "" {
		runtimeBin = detectContainerRuntime()
	}
	if runtimeBin == "" {
		return fmt.Errorf("no container runtime found (tried docker, podman)")
	}

	harnesses := e.harnesses
	if len(harnesses) == 0 {
		harnesses = []string{"claude", "gemini", "opencode", "codex"}
	}

	log.Debug("Starting pull-images",
		"runtime", runtimeBin, "registry", registry, "tag", tag,
		"harnesses", fmt.Sprint(harnesses))

	fmt.Fprintf(logger, "Using runtime: %s\n", runtimeBin)
	fmt.Fprintf(logger, "Registry: %s, Tag: %s\n", registry, tag)
	fmt.Fprintf(logger, "Pulling %d image(s)...\n\n", len(harnesses))

	pulled := 0
	var lastErr error
	for _, h := range harnesses {
		image := fmt.Sprintf("%s/scion-%s:%s", registry, h, tag)
		fmt.Fprintf(logger, "Pulling %s ...\n", image)
		log.Debug("Pulling image", "image", image)

		cmd := exec.CommandContext(ctx, runtimeBin, "image", "pull", image)
		cmd.Stdout = logger
		cmd.Stderr = logger
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(logger, "  ERROR: %v\n\n", err)
			log.Error("Image pull failed", "image", image, "error", err)
			lastErr = err
			continue
		}
		fmt.Fprintf(logger, "  OK\n\n")
		log.Debug("Image pulled successfully", "image", image)
		pulled++
	}

	fmt.Fprintf(logger, "Pull complete: %d/%d succeeded\n", pulled, len(harnesses))
	if lastErr != nil && pulled == 0 {
		return fmt.Errorf("all image pulls failed; last error: %w", lastErr)
	}
	log.Info("Pull images complete", "pulled", pulled, "total", len(harnesses))
	return nil
}

// detectContainerRuntime finds an available container CLI on the system.
func detectContainerRuntime() string {
	for _, bin := range []string{"docker", "podman"} {
		if p, err := exec.LookPath(bin); err == nil && p != "" {
			return bin
		}
	}
	return ""
}

// RebuildServerExecutor rebuilds the server binary from git and restarts via systemd.
type RebuildServerExecutor struct {
	repoPath    string // path to scion source checkout
	repoBranch  string // git branch to checkout before building (empty = stay on current)
	binaryDest  string // install path (e.g., /usr/local/bin/scion)
	serviceName string // systemd service name (e.g., "scion-hub")
}

func (e *RebuildServerExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	log := logging.Subsystem("hub.maintenance.rebuild-server")

	if runtime.GOOS != "linux" {
		return fmt.Errorf("rebuild-server is only supported on Linux (requires systemd); restart the server manually on %s", runtime.GOOS)
	}

	repoPath := e.repoPath
	if repoPath == "" {
		return fmt.Errorf("no repository path configured for rebuild-server")
	}
	binaryDest := e.binaryDest
	if binaryDest == "" {
		binaryDest = "/usr/local/bin/scion"
	}
	serviceName := e.serviceName
	if serviceName == "" {
		serviceName = "scion-hub"
	}

	// A "branch" param on the API request overrides the configured default.
	branch := e.repoBranch
	if v := params["branch"]; v != "" {
		branch = v
	}

	log.Debug("Starting rebuild-server",
		"repo_path", repoPath, "branch", branch,
		"binary_dest", binaryDest, "service_name", serviceName)

	// Build to a staging path inside the repo directory (where the service user
	// has write access), then use "sudo install" to place it into the final
	// destination (e.g., /usr/local/bin/scion). This avoids two problems:
	//   1. ETXTBSY — writing directly to a running binary fails on Linux.
	//   2. Permission denied — the service user typically cannot write to
	//      /usr/local/bin/.
	// Both the install and restart steps use sudo, backed by narrowly-scoped
	// sudoers rules installed by the deploy script (gce-start-hub.sh).
	stagingBinary := filepath.Join(repoPath, "scion.rebuild")

	type step struct {
		name string
		cmd  string
		args []string
		dir  string
	}

	var steps []step
	steps = append(steps, step{"Fetching latest code", "git", []string{"fetch", "origin"}, repoPath})
	if branch != "" {
		steps = append(steps, step{"Checking out branch " + branch, "git", []string{"checkout", branch}, repoPath})
		steps = append(steps, step{"Pulling latest code", "git", []string{"pull", "origin", branch}, repoPath})
	} else {
		steps = append(steps, step{"Pulling latest code", "git", []string{"pull"}, repoPath})
	}
	steps = append(steps,
		step{"Building web assets", "make", []string{"web"}, repoPath},
		step{"Building server binary", "go", []string{"build", "-o", stagingBinary, "./cmd/scion"}, repoPath},
		step{"Installing server binary", "sudo", []string{"install", "-m", "755", stagingBinary, binaryDest}, ""},
	)

	for i, step := range steps {
		fmt.Fprintf(logger, "==> %s\n", step.name)
		log.Debug("Executing step",
			"step", i+1, "name", step.name,
			"cmd", step.cmd, "args", fmt.Sprint(step.args), "dir", step.dir)
		cmd := exec.CommandContext(ctx, step.cmd, step.args...)
		if step.dir != "" {
			cmd.Dir = step.dir
		}
		cmd.Stdout = logger
		cmd.Stderr = logger
		if err := cmd.Run(); err != nil {
			log.Error("Step failed",
				"step", i+1, "name", step.name,
				"cmd", step.cmd, "args", fmt.Sprint(step.args), "error", err)
			return fmt.Errorf("%s failed: %w", step.name, err)
		}
		log.Debug("Step completed", "step", i+1, "name", step.name)
		fmt.Fprintln(logger)
	}

	// Fire-and-forget: start the restart but don't wait for it to finish.
	// "systemctl restart" sends SIGTERM to this very process, so cmd.Run()
	// would never return — it reports "signal: terminated". Using cmd.Start()
	// lets us return success so the calling goroutine can persist the
	// completed run status to the DB before the process is killed.
	fmt.Fprintf(logger, "==> Restarting service\n")
	log.Debug("Initiating service restart (fire-and-forget)",
		"cmd", "sudo", "args", fmt.Sprintf("[systemctl restart %s]", serviceName))
	restartCmd := exec.Command("sudo", "systemctl", "restart", serviceName)
	restartCmd.Stdout = logger
	restartCmd.Stderr = logger
	if err := restartCmd.Start(); err != nil {
		log.Error("Failed to initiate service restart", "error", err)
		return fmt.Errorf("restarting service failed: %w", err)
	}

	log.Info("Server rebuild complete, restart initiated")
	fmt.Fprintln(logger, "\nServer rebuild complete, restart initiated.")
	return nil
}

// RebuildWebExecutor rebuilds the web frontend assets from source.
type RebuildWebExecutor struct {
	repoPath   string // path to scion source checkout
	repoBranch string // git branch to checkout before building (empty = stay on current)
}

func (e *RebuildWebExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	log := logging.Subsystem("hub.maintenance.rebuild-web")

	repoPath := e.repoPath
	if repoPath == "" {
		return fmt.Errorf("no repository path configured for rebuild-web")
	}

	branch := e.repoBranch
	if v := params["branch"]; v != "" {
		branch = v
	}

	log.Debug("Starting rebuild-web", "repo_path", repoPath, "branch", branch)

	type step struct {
		name string
		cmd  string
		args []string
	}

	var steps []step
	steps = append(steps, step{"Fetching latest code", "git", []string{"fetch", "origin"}})
	if branch != "" {
		steps = append(steps, step{"Checking out branch " + branch, "git", []string{"checkout", branch}})
		steps = append(steps, step{"Pulling latest code", "git", []string{"pull", "origin", branch}})
	} else {
		steps = append(steps, step{"Pulling latest code", "git", []string{"pull"}})
	}
	steps = append(steps, step{"Building web assets", "make", []string{"web"}})

	for i, step := range steps {
		fmt.Fprintf(logger, "==> %s\n", step.name)
		log.Debug("Executing step",
			"step", i+1, "name", step.name,
			"cmd", step.cmd, "args", fmt.Sprint(step.args))
		cmd := exec.CommandContext(ctx, step.cmd, step.args...)
		cmd.Dir = repoPath
		cmd.Stdout = logger
		cmd.Stderr = logger
		if err := cmd.Run(); err != nil {
			log.Error("Step failed",
				"step", i+1, "name", step.name, "error", err)
			return fmt.Errorf("%s failed: %w", step.name, err)
		}
		log.Debug("Step completed", "step", i+1, "name", step.name)
		fmt.Fprintln(logger)
	}

	log.Info("Web frontend rebuild complete")
	fmt.Fprintln(logger, "Web frontend rebuild complete. Changes take effect on the next page load.")
	return nil
}

// RebuildContainerBinariesExecutor rebuilds scion and sciontool binaries
// for bind-mounting into agent containers via SCION_DEV_BINARIES.
type RebuildContainerBinariesExecutor struct {
	repoPath string
}

func (e *RebuildContainerBinariesExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	log := logging.Subsystem("hub.maintenance.rebuild-container-binaries")

	if e.repoPath == "" {
		return fmt.Errorf("no repository path configured for rebuild-container-binaries")
	}

	devBinDir := os.Getenv("SCION_DEV_BINARIES")
	fmt.Fprintf(logger, "SCION_DEV_BINARIES=%s\n", devBinDir)
	if devBinDir == "" {
		fmt.Fprintln(logger, "WARNING: SCION_DEV_BINARIES is not set; built binaries will not be mounted into containers until it is configured.")
	}

	log.Debug("Starting rebuild-container-binaries", "repo_path", e.repoPath)

	fmt.Fprintf(logger, "==> Building container binaries\n")
	cmd := exec.CommandContext(ctx, "make", "container-binaries")
	cmd.Dir = e.repoPath
	cmd.Stdout = logger
	cmd.Stderr = logger
	if err := cmd.Run(); err != nil {
		log.Error("Build failed", "error", err)
		return fmt.Errorf("make container-binaries failed: %w", err)
	}

	log.Info("Container binaries rebuild complete")
	fmt.Fprintln(logger, "\nContainer binaries rebuild complete.")
	return nil
}

// UpdateCheckResult contains the result of a check-for-updates operation.
type UpdateCheckResult struct {
	UpdateAvailable bool               `json:"update_available"`
	CurrentCommit   string             `json:"current_commit"`
	LatestCommit    string             `json:"latest_commit"`
	CurrentBranch   string             `json:"current_branch"`
	TrackingRef     string             `json:"tracking_ref"`
	CommitsBehind   int                `json:"commits_behind"`
	NewCommits      []UpdateCommitInfo `json:"new_commits,omitempty"`
}

// UpdateCommitInfo describes a single commit available in the remote.
type UpdateCommitInfo struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
}

// CheckForUpdates fetches the remote and compares the local HEAD against
// the current branch's remote tracking ref to determine whether a newer
// version is available.
func CheckForUpdates(ctx context.Context, repoPath string) (*UpdateCheckResult, error) {
	log := logging.Subsystem("hub.maintenance.check-updates")

	if repoPath == "" {
		return nil, fmt.Errorf("no repository path configured")
	}

	log.Debug("Checking for updates", "repo_path", repoPath)

	// Fetch from origin.
	fetchCmd := exec.CommandContext(ctx, "git", "fetch", "origin")
	fetchCmd.Dir = repoPath
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		log.Error("git fetch failed", "error", err, "output", string(out))
		return nil, fmt.Errorf("git fetch failed: %w", err)
	}

	// Detect the current branch.
	branchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = repoPath
	branchOut, err := branchCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get current branch: %w", err)
	}
	currentBranch := strings.TrimSpace(string(branchOut))

	// Resolve the remote tracking ref (e.g., "origin/main" or "origin/feature-branch").
	trackingRef := "origin/" + currentBranch

	// Get local HEAD commit.
	localCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	localCmd.Dir = repoPath
	localOut, err := localCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get local HEAD: %w", err)
	}
	localCommit := strings.TrimSpace(string(localOut))

	// Get remote tracking commit.
	remoteCmd := exec.CommandContext(ctx, "git", "rev-parse", trackingRef)
	remoteCmd.Dir = repoPath
	remoteOut, err := remoteCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get %s: %w", trackingRef, err)
	}
	remoteCommit := strings.TrimSpace(string(remoteOut))

	result := &UpdateCheckResult{
		CurrentCommit: localCommit,
		LatestCommit:  remoteCommit,
		CurrentBranch: currentBranch,
		TrackingRef:   trackingRef,
	}

	if localCommit == remoteCommit {
		return result, nil
	}

	result.UpdateAvailable = true

	// Get list of new commits (local..remote).
	logCmd := exec.CommandContext(ctx, "git", "log", "--oneline", "--no-decorate",
		localCommit+".."+remoteCommit)
	logCmd.Dir = repoPath
	logOut, err := logCmd.Output()
	if err != nil {
		// Non-fatal: we know there's an update but can't list commits.
		log.Warn("Failed to list new commits", "error", err)
		return result, nil
	}

	lines := strings.Split(strings.TrimSpace(string(logOut)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		info := UpdateCommitInfo{Hash: parts[0]}
		if len(parts) > 1 {
			info.Subject = parts[1]
		}
		result.NewCommits = append(result.NewCommits, info)
	}
	result.CommitsBehind = len(result.NewCommits)

	log.Info("Update check complete",
		"update_available", true,
		"commits_behind", result.CommitsBehind,
		"branch", currentBranch,
		"tracking_ref", trackingRef,
		"current", localCommit[:8],
		"latest", remoteCommit[:8])

	return result, nil
}

// parseMigrationParams extracts and validates migration-specific parameters from the request body.
func parseMigrationParams(body map[string]interface{}) map[string]string {
	params := make(map[string]string)
	if raw, ok := body["params"]; ok {
		if m, ok := raw.(map[string]interface{}); ok {
			for k, v := range m {
				switch k {
				case "dryRun":
					if b, ok := v.(bool); ok && b {
						params["dryRun"] = "true"
					}
				default:
					if s, ok := v.(string); ok {
						params[strings.TrimSpace(k)] = strings.TrimSpace(s)
					}
				}
			}
		}
	}
	return params
}
