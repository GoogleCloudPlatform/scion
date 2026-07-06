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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cloudbuildpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	gcstorage "cloud.google.com/go/storage"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"google.golang.org/protobuf/types/known/durationpb"
	"gopkg.in/yaml.v3"
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
		_, _ = fmt.Fprintln(logger, "No secrets found to migrate.")
		return nil
	}

	_, _ = fmt.Fprintf(logger, "Found %d secret(s) to process.\n", len(allSecrets))
	if dryRun {
		_, _ = fmt.Fprintln(logger, "DRY RUN: No changes will be made.")
	}

	migrated := 0
	skipped := 0

	for _, s := range allSecrets {
		// Skip secrets that already have a GCP SM reference.
		if s.SecretRef != "" {
			_, _ = fmt.Fprintf(logger, "  SKIP  %s (scope: %s/%s) - already has ref: %s\n", s.Key, s.Scope, s.ScopeID, s.SecretRef)
			skipped++
			continue
		}

		if dryRun {
			_, _ = fmt.Fprintf(logger, "  WOULD MIGRATE  %s (scope: %s/%s, type: %s)\n", s.Key, s.Scope, s.ScopeID, s.SecretType)
			migrated++
			continue
		}

		// Read value from the database.
		value, err := e.store.GetSecretValue(ctx, s.Key, s.Scope, s.ScopeID)
		if err != nil {
			_, _ = fmt.Fprintf(logger, "  WARN  %s (scope: %s/%s) - failed to get value: %v\n", s.Key, s.Scope, s.ScopeID, err)
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
			_, _ = fmt.Fprintf(logger, "  ERROR  %s (scope: %s/%s) - %v\n", s.Key, s.Scope, s.ScopeID, err)
			skipped++
			continue
		}

		_, _ = fmt.Fprintf(logger, "  MIGRATED  %s (scope: %s/%s, type: %s)\n", s.Key, s.Scope, s.ScopeID, s.SecretType)
		migrated++
	}

	status := "complete"
	if dryRun {
		status = "dry run complete"
	}
	_, _ = fmt.Fprintf(logger, "\nMigration %s: %d migrated, %d skipped\n", status, migrated, skipped)

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
		runtimeBin = scionruntime.DetectContainerRuntime()
	}
	if runtimeBin == "" {
		return fmt.Errorf("no container runtime found (tried docker, podman, container)")
	}

	harnesses := e.harnesses
	if len(harnesses) == 0 {
		harnesses = []string{"claude", "gemini"}
	}

	log.Debug("Starting pull-images",
		"runtime", runtimeBin, "registry", registry, "tag", tag,
		"harnesses", fmt.Sprint(harnesses))

	_, _ = fmt.Fprintf(logger, "Using runtime: %s\n", runtimeBin)
	_, _ = fmt.Fprintf(logger, "Registry: %s, Tag: %s\n", registry, tag)
	_, _ = fmt.Fprintf(logger, "Pulling %d image(s)...\n\n", len(harnesses))

	pulled := 0
	var lastErr error
	for _, h := range harnesses {
		image := fmt.Sprintf("%s/scion-%s:%s", registry, h, tag)
		_, _ = fmt.Fprintf(logger, "Pulling %s ...\n", image)
		log.Debug("Pulling image", "image", image)

		cmd := exec.CommandContext(ctx, runtimeBin, "image", "pull", image)
		cmd.Stdout = logger
		cmd.Stderr = logger
		if err := cmd.Run(); err != nil {
			_, _ = fmt.Fprintf(logger, "  ERROR: %v\n\n", err)
			log.Error("Image pull failed", "image", image, "error", err)
			lastErr = err
			continue
		}
		_, _ = fmt.Fprintf(logger, "  OK\n\n")
		log.Debug("Image pulled successfully", "image", image)
		pulled++
	}

	_, _ = fmt.Fprintf(logger, "Pull complete: %d/%d succeeded\n", pulled, len(harnesses))
	if lastErr != nil && pulled == 0 {
		return fmt.Errorf("all image pulls failed; last error: %w", lastErr)
	}
	log.Info("Pull images complete", "pulled", pulled, "total", len(harnesses))
	return nil
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
		steps = append(steps, step{"Resetting to origin/" + branch, "git", []string{"reset", "--hard", "origin/" + branch}, repoPath})
	} else {
		// Determine the current branch to reset against its remote tracking ref.
		branchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
		branchCmd.Dir = repoPath
		branchOut, err := branchCmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to determine current branch: %s", strings.TrimSpace(string(branchOut)))
		}
		currentBranch := strings.TrimSpace(string(branchOut))
		steps = append(steps, step{"Resetting to origin/" + currentBranch, "git", []string{"reset", "--hard", "origin/" + currentBranch}, repoPath})
	}
	steps = append(steps,
		step{"Building web assets", "make", []string{"web"}, repoPath},
		step{"Building server binary", "go", []string{"build", "-o", stagingBinary, "./cmd/scion"}, repoPath},
		step{"Installing server binary", "sudo", []string{"install", "-m", "755", stagingBinary, binaryDest}, ""},
	)

	for i, step := range steps {
		_, _ = fmt.Fprintf(logger, "==> %s\n", step.name)
		log.Debug("Executing step",
			"step", i+1, "name", step.name,
			"cmd", step.cmd, "args", fmt.Sprint(step.args), "dir", step.dir)
		cmd := exec.CommandContext(ctx, step.cmd, step.args...)
		if step.dir != "" {
			cmd.Dir = step.dir
		}
		var stderr bytes.Buffer
		cmd.Stdout = logger
		cmd.Stderr = io.MultiWriter(logger, &stderr)
		if err := cmd.Run(); err != nil {
			errDetail := strings.TrimSpace(stderr.String())
			log.Error("Step failed",
				"step", i+1, "name", step.name,
				"cmd", step.cmd, "args", fmt.Sprint(step.args),
				"error", err, "stderr", errDetail)
			if errDetail != "" {
				return fmt.Errorf("%s failed: %s", step.name, errDetail)
			}
			return fmt.Errorf("%s failed: %w", step.name, err)
		}
		log.Debug("Step completed", "step", i+1, "name", step.name)
		_, _ = fmt.Fprintln(logger)
	}

	// Fire-and-forget: start the restart but don't wait for it to finish.
	// "systemctl restart" sends SIGTERM to this very process, so cmd.Run()
	// would never return — it reports "signal: terminated". Using cmd.Start()
	// lets us return success so the calling goroutine can persist the
	// completed run status to the DB before the process is killed.
	_, _ = fmt.Fprintf(logger, "==> Restarting service\n")
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
	_, _ = fmt.Fprintln(logger, "\nServer rebuild complete, restart initiated.")
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
		steps = append(steps, step{"Resetting to origin/" + branch, "git", []string{"reset", "--hard", "origin/" + branch}})
	} else {
		branchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
		branchCmd.Dir = repoPath
		branchOut, err := branchCmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to determine current branch: %s", strings.TrimSpace(string(branchOut)))
		}
		currentBranch := strings.TrimSpace(string(branchOut))
		steps = append(steps, step{"Resetting to origin/" + currentBranch, "git", []string{"reset", "--hard", "origin/" + currentBranch}})
	}
	steps = append(steps, step{"Building web assets", "make", []string{"web"}})

	for i, step := range steps {
		_, _ = fmt.Fprintf(logger, "==> %s\n", step.name)
		log.Debug("Executing step",
			"step", i+1, "name", step.name,
			"cmd", step.cmd, "args", fmt.Sprint(step.args))
		cmd := exec.CommandContext(ctx, step.cmd, step.args...)
		cmd.Dir = repoPath
		var stderr bytes.Buffer
		cmd.Stdout = logger
		cmd.Stderr = io.MultiWriter(logger, &stderr)
		if err := cmd.Run(); err != nil {
			errDetail := strings.TrimSpace(stderr.String())
			log.Error("Step failed",
				"step", i+1, "name", step.name,
				"error", err, "stderr", errDetail)
			if errDetail != "" {
				return fmt.Errorf("%s failed: %s", step.name, errDetail)
			}
			return fmt.Errorf("%s failed: %w", step.name, err)
		}
		log.Debug("Step completed", "step", i+1, "name", step.name)
		_, _ = fmt.Fprintln(logger)
	}

	log.Info("Web frontend rebuild complete")
	_, _ = fmt.Fprintln(logger, "Web frontend rebuild complete. Changes take effect on the next page load.")
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
	_, _ = fmt.Fprintf(logger, "SCION_DEV_BINARIES=%s\n", devBinDir)
	if devBinDir == "" {
		_, _ = fmt.Fprintln(logger, "WARNING: SCION_DEV_BINARIES is not set; built binaries will not be mounted into containers until it is configured.")
	}

	log.Debug("Starting rebuild-container-binaries", "repo_path", e.repoPath)

	_, _ = fmt.Fprintf(logger, "==> Building container binaries\n")
	cmd := exec.CommandContext(ctx, "make", "container-binaries")
	cmd.Dir = e.repoPath
	cmd.Stdout = logger
	cmd.Stderr = logger
	if err := cmd.Run(); err != nil {
		log.Error("Build failed", "error", err)
		return fmt.Errorf("make container-binaries failed: %w", err)
	}

	log.Info("Container binaries rebuild complete")
	_, _ = fmt.Fprintln(logger, "\nContainer binaries rebuild complete.")
	return nil
}

// BuildHarnessConfigImageExecutor builds a container image from a harness-config's Dockerfile.
type BuildHarnessConfigImageExecutor struct {
	store      store.Store
	storage    storage.Storage
	runtimeBin string
	registry   string
	tag        string
	gcpProject string
}

func (e *BuildHarnessConfigImageExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	if params["builder"] == "cloud-build" {
		cloudExecutor := &CloudBuildHarnessConfigExecutor{
			store:      e.store,
			storage:    e.storage,
			gcpProject: e.gcpProject,
			registry:   e.registry,
			tag:        e.tag,
		}
		return cloudExecutor.Run(ctx, logger, params)
	}

	log := logging.Subsystem("hub.maintenance.build-harness-config-image")

	harnessConfigID := params["harness_config_id"]
	if harnessConfigID == "" {
		return fmt.Errorf("missing required parameter: harness_config_id")
	}

	tag := e.tag
	if tag == "" {
		tag = "latest"
	}
	if v := params["tag"]; v != "" {
		tag = v
	}

	registry := e.registry
	if v := params["registry"]; v != "" {
		registry = v
	}
	registry = strings.TrimSuffix(registry, "/")

	hc, err := e.store.GetHarnessConfig(ctx, harnessConfigID)
	if err != nil {
		return fmt.Errorf("failed to load harness-config %q: %w", harnessConfigID, err)
	}

	hasDockerfile := false
	for _, f := range hc.Files {
		if f.Path == "Dockerfile" {
			hasDockerfile = true
			break
		}
	}
	if !hasDockerfile {
		return fmt.Errorf("harness-config %q does not contain a Dockerfile", hc.Name)
	}

	if e.storage == nil {
		return fmt.Errorf("storage not configured")
	}

	tmpDir, err := os.MkdirTemp("", "scion-build-*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	if err := materializeHarnessConfigFiles(ctx, e.storage, hc, tmpDir, logger); err != nil {
		return err
	}

	baseImage := "scion-base:" + tag
	if registry != "" {
		baseImage = registry + "/scion-base:" + tag
	}
	_, _ = fmt.Fprintf(logger, "Base image: %s\n", baseImage)

	runtimeBin := e.runtimeBin
	if runtimeBin == "" {
		runtimeBin = scionruntime.DetectContainerRuntime()
	}
	if runtimeBin == "" {
		return fmt.Errorf("no container runtime found (tried docker, podman, container)")
	}

	imageName := hc.Slug
	if imageName == "" {
		imageName = hc.Name
	}
	outputImage := imageName + ":" + tag
	_, _ = fmt.Fprintf(logger, "Building %s from harness-config %q...\n", outputImage, hc.Name)
	log.Debug("Starting container build",
		"image", outputImage, "base_image", baseImage,
		"runtime", runtimeBin, "harness_config", hc.Name)

	cmd := exec.CommandContext(ctx, runtimeBin, "build",
		"--build-arg", "BASE_IMAGE="+baseImage,
		"-t", outputImage,
		tmpDir)
	cmd.Stdout = logger
	cmd.Stderr = logger
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build failed: %w", err)
	}

	if params["push"] == "true" && registry != "" {
		pushImage := registry + "/" + outputImage
		_, _ = fmt.Fprintf(logger, "Tagging %s as %s...\n", outputImage, pushImage)
		tagCmd := exec.CommandContext(ctx, runtimeBin, "image", "tag", outputImage, pushImage)
		tagCmd.Stdout = logger
		tagCmd.Stderr = logger
		if err := tagCmd.Run(); err != nil {
			return fmt.Errorf("tag failed: %w", err)
		}

		_, _ = fmt.Fprintf(logger, "Pushing %s...\n", pushImage)
		pushCmd := exec.CommandContext(ctx, runtimeBin, "image", "push", pushImage)
		pushCmd.Stdout = logger
		pushCmd.Stderr = logger
		if err := pushCmd.Run(); err != nil {
			return fmt.Errorf("push failed: %w", err)
		}
		outputImage = pushImage
	}

	// Update the harness config's image in storage and the DB so agents
	// pick up the newly-built image instead of the stale upstream reference.
	if err := syncBuiltImage(ctx, e.storage, e.store, logger, hc, tmpDir, outputImage); err != nil {
		log.Error("Failed to sync built image back to store", "error", err)
		_, _ = fmt.Fprintf(logger, "Warning: build succeeded but failed to update harness-config image: %v\n", err)
	}

	_, _ = fmt.Fprintf(logger, "\nBuild complete: %s\n", outputImage)
	log.Info("Build complete", "image", outputImage, "harness_config", hc.Name)
	return nil
}

// materializeHarnessConfigFiles downloads all files from a harness config's
// storage path into the given directory. It is shared by both the local and
// Cloud Build executors.
func materializeHarnessConfigFiles(ctx context.Context, stor storage.Storage, hc *store.HarnessConfig, destDir string, logger io.Writer) error {
	_, _ = fmt.Fprintf(logger, "Materializing %d file(s) from harness-config %q...\n", len(hc.Files), hc.Name)
	for _, f := range hc.Files {
		objectPath := hc.StoragePath + "/" + f.Path
		reader, _, err := stor.Download(ctx, objectPath)
		if err != nil {
			return fmt.Errorf("failed to download %q from storage: %w", f.Path, err)
		}

		destPath := filepath.Join(destDir, f.Path)
		if !strings.HasPrefix(destPath, destDir+string(os.PathSeparator)) {
			_ = reader.Close()
			return fmt.Errorf("invalid file path %q: escapes build directory", f.Path)
		}
		if dir := filepath.Dir(destPath); dir != destDir {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				_ = reader.Close()
				return fmt.Errorf("failed to create directory for %q: %w", f.Path, err)
			}
		}

		outFile, err := os.Create(destPath)
		if err != nil {
			_ = reader.Close()
			return fmt.Errorf("failed to create file %q: %w", f.Path, err)
		}
		_, err = io.Copy(outFile, reader)
		_ = reader.Close()
		_ = outFile.Close()
		if err != nil {
			return fmt.Errorf("failed to write file %q: %w", f.Path, err)
		}

		if f.Mode != "" {
			mode := os.FileMode(0o644)
			if _, err := fmt.Sscanf(f.Mode, "%o", &mode); err == nil {
				_ = os.Chmod(destPath, mode)
			}
		}
	}
	return nil
}

// CloudBuildHarnessConfigExecutor builds a harness-config image using Google Cloud Build.
type CloudBuildHarnessConfigExecutor struct {
	store      store.Store
	storage    storage.Storage
	gcpProject string
	registry   string
	tag        string
}

func (e *CloudBuildHarnessConfigExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	log := logging.Subsystem("hub.maintenance.cloud-build-harness-config-image")

	harnessConfigID := params["harness_config_id"]
	if harnessConfigID == "" {
		return fmt.Errorf("missing required parameter: harness_config_id")
	}

	if e.gcpProject == "" {
		return fmt.Errorf("cloud Build requires a GCP project ID, configure gcp_project_id in settings")
	}

	tag := e.tag
	if tag == "" {
		tag = "latest"
	}
	if v := params["tag"]; v != "" {
		tag = v
	}

	registry := e.registry
	if v := params["registry"]; v != "" {
		registry = v
	}
	registry = strings.TrimSuffix(registry, "/")
	if registry == "" {
		return fmt.Errorf("cloud Build requires a registry (images must be pushed), configure image_registry first")
	}

	hc, err := e.store.GetHarnessConfig(ctx, harnessConfigID)
	if err != nil {
		return fmt.Errorf("failed to load harness-config %q: %w", harnessConfigID, err)
	}

	hasDockerfile := false
	for _, f := range hc.Files {
		if f.Path == "Dockerfile" {
			hasDockerfile = true
			break
		}
	}
	if !hasDockerfile {
		return fmt.Errorf("harness-config %q does not contain a Dockerfile", hc.Name)
	}

	if e.storage == nil {
		return fmt.Errorf("storage not configured")
	}

	// Materialize harness-config files to a temp directory.
	tmpDir, err := os.MkdirTemp("", "scion-cloudbuild-*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	if err := materializeHarnessConfigFiles(ctx, e.storage, hc, tmpDir, logger); err != nil {
		return err
	}

	// Create a tar.gz of the build context.
	_, _ = fmt.Fprintln(logger, "Creating build context archive...")
	var buf bytes.Buffer
	if err := createTarGz(tmpDir, &buf); err != nil {
		return fmt.Errorf("failed to create build context archive: %w", err)
	}

	// Upload build context to the Cloud Build default staging bucket.
	stagingBucket := e.gcpProject + "_cloudbuild"
	objectName := fmt.Sprintf("source/scion-harness-%s-%d.tar.gz", hc.Name, time.Now().Unix())
	_, _ = fmt.Fprintf(logger, "Uploading build context to gs://%s/%s...\n", stagingBucket, objectName)

	gcsClient, err := gcstorage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create GCS client: %w", err)
	}
	defer func() { _ = gcsClient.Close() }()

	// Verify the staging bucket exists before attempting upload.
	if _, err := gcsClient.Bucket(stagingBucket).Attrs(ctx); err != nil {
		if err == gcstorage.ErrBucketNotExist {
			return fmt.Errorf("cloud Build staging bucket gs://%s does not exist; "+
				"run 'gcloud builds submit' once manually in project %s to create it, "+
				"or create the bucket manually", stagingBucket, e.gcpProject)
		}
		return fmt.Errorf("failed to access staging bucket gs://%s: %w", stagingBucket, err)
	}

	wc := gcsClient.Bucket(stagingBucket).Object(objectName).NewWriter(ctx)
	wc.ContentType = "application/gzip"
	if _, err := io.Copy(wc, &buf); err != nil {
		_ = wc.Close()
		return fmt.Errorf("failed to upload build context: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("failed to finalize build context upload: %w", err)
	}
	_, _ = fmt.Fprintln(logger, "Build context uploaded.")

	// Clean up the source archive after the build completes (success or failure).
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := gcsClient.Bucket(stagingBucket).Object(objectName).Delete(cleanupCtx); err != nil {
			log.Warn("Failed to clean up Cloud Build source archive", "bucket", stagingBucket, "object", objectName, "error", err)
		}
	}()

	// Build the output image reference.
	imageName := hc.Slug
	if imageName == "" {
		imageName = hc.Name
	}
	baseImage := registry + "/scion-base:" + tag
	outputImage := registry + "/" + imageName + ":" + tag

	_, _ = fmt.Fprintf(logger, "Base image: %s\n", baseImage)
	_, _ = fmt.Fprintf(logger, "Output image: %s\n", outputImage)
	_, _ = fmt.Fprintf(logger, "Submitting Cloud Build job in project %q...\n", e.gcpProject)
	log.Debug("Submitting Cloud Build",
		"image", outputImage, "base_image", baseImage,
		"project", e.gcpProject, "harness_config", hc.Name)

	// Submit Cloud Build.
	cbClient, err := cloudbuild.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create Cloud Build client: %w", err)
	}
	defer func() { _ = cbClient.Close() }()

	build := &cloudbuildpb.Build{
		Source: &cloudbuildpb.Source{
			Source: &cloudbuildpb.Source_StorageSource{
				StorageSource: &cloudbuildpb.StorageSource{
					Bucket: stagingBucket,
					Object: objectName,
				},
			},
		},
		Steps: []*cloudbuildpb.BuildStep{
			{
				Name: "gcr.io/cloud-builders/docker",
				Args: []string{"buildx", "create", "--name", "builder", "--use"},
				Id:   "setup-buildx",
				Env:  []string{"DOCKER_CLI_EXPERIMENTAL=enabled"},
			},
			{
				Name: "gcr.io/cloud-builders/docker",
				Args: []string{"buildx", "inspect", "--bootstrap"},
				Id:   "bootstrap-buildx",
				Env:  []string{"DOCKER_CLI_EXPERIMENTAL=enabled"},
			},
			{
				Name: "gcr.io/cloud-builders/docker",
				Args: []string{
					"buildx", "build",
					"--platform", "linux/amd64,linux/arm64",
					"--build-arg", "BASE_IMAGE=" + baseImage,
					"-t", outputImage,
					"-f", "Dockerfile",
					"--push",
					".",
				},
				Id:  "build-image",
				Env: []string{"DOCKER_CLI_EXPERIMENTAL=enabled"},
			},
		},
		Options: &cloudbuildpb.BuildOptions{
			MachineType:          cloudbuildpb.BuildOptions_E2_HIGHCPU_8,
			DynamicSubstitutions: true,
		},
		Timeout: durationpb.New(20 * time.Minute),
	}

	op, err := cbClient.CreateBuild(ctx, &cloudbuildpb.CreateBuildRequest{
		ProjectId: e.gcpProject,
		Build:     build,
	})
	if err != nil {
		return fmt.Errorf("failed to submit Cloud Build: %w", err)
	}

	buildID, err := extractBuildID(op)
	if err != nil {
		return fmt.Errorf("failed to get build ID from operation: %w", err)
	}
	_, _ = fmt.Fprintf(logger, "Cloud Build started: %s\n", buildID)
	log.Info("Cloud Build started", "build_id", buildID, "project", e.gcpProject)

	// Poll for build status and stream logs.
	logBucket := e.gcpProject + "_cloudbuild"
	logObjectPrefix := fmt.Sprintf("log-%s", buildID)
	var lastLogOffset int64

	for {
		b, err := cbClient.GetBuild(ctx, &cloudbuildpb.GetBuildRequest{
			ProjectId: e.gcpProject,
			Id:        buildID,
		})
		if err != nil {
			return fmt.Errorf("failed to poll build status: %w", err)
		}

		// Stream new log content from GCS.
		lastLogOffset = streamCloudBuildLogs(ctx, gcsClient, logBucket, logObjectPrefix+".txt", lastLogOffset, logger)

		switch b.Status {
		case cloudbuildpb.Build_SUCCESS:
			// Final log flush.
			streamCloudBuildLogs(ctx, gcsClient, logBucket, logObjectPrefix+".txt", lastLogOffset, logger)
			_, _ = fmt.Fprintf(logger, "\nCloud Build completed successfully: %s\n", outputImage)
			log.Info("Cloud Build complete", "build_id", buildID, "image", outputImage)

			if err := syncBuiltImage(ctx, e.storage, e.store, logger, hc, tmpDir, outputImage); err != nil {
				log.Error("Failed to sync built image back to store", "error", err)
				_, _ = fmt.Fprintf(logger, "Warning: build succeeded but failed to update harness-config image: %v\n", err)
			}
			return nil

		case cloudbuildpb.Build_FAILURE:
			streamCloudBuildLogs(ctx, gcsClient, logBucket, logObjectPrefix+".txt", lastLogOffset, logger)
			return fmt.Errorf("cloud Build failed: %s", b.StatusDetail)

		case cloudbuildpb.Build_TIMEOUT:
			return fmt.Errorf("cloud Build timed out")

		case cloudbuildpb.Build_CANCELLED:
			return fmt.Errorf("cloud Build was cancelled")

		case cloudbuildpb.Build_INTERNAL_ERROR:
			return fmt.Errorf("cloud Build internal error: %s", b.StatusDetail)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// syncBuiltImage updates the harness config's config.yaml in storage and the
// DB record to reference the newly-built image. Shared by both local and Cloud
// Build executors.
func syncBuiltImage(ctx context.Context, stor storage.Storage, storeDB store.Store, logger io.Writer, hc *store.HarnessConfig, tmpDir, outputImage string) error {
	configPath := filepath.Join(tmpDir, "config.yaml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("failed to read config.yaml: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(configData, &doc); err != nil {
		return fmt.Errorf("failed to parse config.yaml: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("config.yaml root is not a YAML mapping")
	}
	{
		mapping := doc.Content[0]
		found := false
		for i := 0; i < len(mapping.Content)-1; i += 2 {
			if mapping.Content[i].Value == "image" {
				mapping.Content[i+1].Value = outputImage
				found = true
				break
			}
		}
		if !found {
			mapping.Content = append(mapping.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "image"},
				&yaml.Node{Kind: yaml.ScalarNode, Value: outputImage},
			)
		}
	}

	updatedData, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("failed to marshal updated config.yaml: %w", err)
	}

	if stor != nil && hc.StoragePath != "" {
		objectPath := hc.StoragePath + "/config.yaml"
		if _, err := stor.Upload(ctx, objectPath, bytes.NewReader(updatedData), storage.UploadOptions{}); err != nil {
			return fmt.Errorf("failed to upload updated config.yaml to storage: %w", err)
		}
		_, _ = fmt.Fprintf(logger, "Updated config.yaml in storage with image %s\n", outputImage)
	}

	configHash := transfer.HashBytes(updatedData)
	for i, f := range hc.Files {
		if f.Path == "config.yaml" {
			hc.Files[i].Size = int64(len(updatedData))
			hc.Files[i].Hash = configHash
			break
		}
	}
	hc.ContentHash = computeContentHash(hc.Files)

	if hc.Config == nil {
		hc.Config = &store.HarnessConfigData{}
	}
	hc.Config.Image = outputImage
	if err := storeDB.UpdateHarnessConfig(ctx, hc); err != nil {
		return fmt.Errorf("failed to update harness-config record: %w", err)
	}
	_, _ = fmt.Fprintf(logger, "Updated harness-config record image to %s\n", outputImage)

	return nil
}

// createTarGz creates a tar.gz archive from the contents of srcDir.
func createTarGz(srcDir string, w io.Writer) error {
	gzw := gzip.NewWriter(w)
	defer func() { _ = gzw.Close() }()
	tw := tar.NewWriter(gzw)
	defer func() { _ = tw.Close() }()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relPath)

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	})
}

// extractBuildID extracts the Cloud Build ID from a CreateBuild long-running
// operation response.
func extractBuildID(op *cloudbuild.CreateBuildOperation) (string, error) {
	meta, err := op.Metadata()
	if err != nil {
		return "", fmt.Errorf("failed to get build operation metadata: %w", err)
	}
	if meta.Build == nil || meta.Build.Id == "" {
		return "", fmt.Errorf("build metadata does not contain a build ID")
	}
	return meta.Build.Id, nil
}

// streamCloudBuildLogs reads new log content from the Cloud Build log object
// in GCS and writes it to the logger. It returns the new offset.
func streamCloudBuildLogs(ctx context.Context, client *gcstorage.Client, bucket, object string, offset int64, logger io.Writer) int64 {
	rc, err := client.Bucket(bucket).Object(object).NewRangeReader(ctx, offset, -1)
	if err != nil {
		return offset
	}
	defer func() { _ = rc.Close() }()

	n, _ := io.Copy(logger, rc)
	return offset + n
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
