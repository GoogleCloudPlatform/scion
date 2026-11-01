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

package maintenance

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cloudbuildpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	gcstorage "cloud.google.com/go/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/unitdrift"
	"github.com/GoogleCloudPlatform/scion/pkg/preflight"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"google.golang.org/protobuf/types/known/durationpb"
)

// MaintenanceExecutor defines the interface for a runnable maintenance operation.
type MaintenanceExecutor interface {
	// Run executes the operation. The context is cancelled if the server shuts down.
	// The logger captures output that is stored in the run/operation's log field.
	// Params contains operation-specific configuration from the API request.
	Run(ctx context.Context, logger io.Writer, params map[string]string) error
}

// HubSettingSectionUpdateAvailable is the hub_settings section key used to
// store the latest detected release update for the admin UI.
const HubSettingSectionUpdateAvailable = "system.update_available"

// HubSettingSectionUnitDrift is the hub_settings section key holding the
// last systemd unit check of Rebuild from git (UnitDriftStatus).
const HubSettingSectionUnitDrift = "system.unit_drift"

// SecretMigrationExecutor migrates hub-scoped secrets from the legacy fixed "hub" scope ID
// to the per-instance hub ID namespace in GCP Secret Manager.
type SecretMigrationExecutor struct {
	store         store.Store
	secretBackend secret.SecretBackend
}

// NewSecretMigrationExecutor returns a SecretMigrationExecutor that reads
// secrets from s and migrates them in backend.
func NewSecretMigrationExecutor(s store.Store, backend secret.SecretBackend) *SecretMigrationExecutor {
	return &SecretMigrationExecutor{
		store:         s,
		secretBackend: backend,
	}
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
	harnesses  []string // harness names (e.g., "claude", "antigravity")
}

// PullImagesOptions configures a PullImagesExecutor. Zero values are kept
// as-is; Run applies its own defaults.
type PullImagesOptions struct {
	RuntimeBin string   // "docker", "podman", or "container"
	Registry   string   // image registry prefix
	Tag        string   // image tag (default "latest")
	Harnesses  []string // harness names (e.g., "claude", "antigravity")
}

// NewPullImagesExecutor returns a PullImagesExecutor configured from opts.
func NewPullImagesExecutor(opts PullImagesOptions) *PullImagesExecutor {
	return &PullImagesExecutor{
		runtimeBin: opts.RuntimeBin,
		registry:   opts.Registry,
		tag:        opts.Tag,
		harnesses:  opts.Harnesses,
	}
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
		harnesses = []string{"claude", "antigravity"}
	}

	// The registry is shown redacted (config.RedactImageRegistry).
	log.Debug("Starting pull-images",
		"runtime", runtimeBin, "registry", config.RedactImageRegistry(registry), "tag", tag,
		"harnesses", fmt.Sprint(harnesses))

	_, _ = fmt.Fprintf(logger, "Using runtime: %s\n", runtimeBin)
	_, _ = fmt.Fprintf(logger, "Registry: %s, Tag: %s\n", config.RedactImageRegistry(registry), tag)
	_, _ = fmt.Fprintf(logger, "Pulling %d image(s)...\n\n", len(harnesses))

	pulled := 0
	var lastErr error
	for _, h := range harnesses {
		image := fmt.Sprintf("%s/scion-%s:%s", registry, h, tag)
		shown := config.RedactImageRegistry(image)
		_, _ = fmt.Fprintf(logger, "Pulling %s ...\n", shown)
		log.Debug("Pulling image", "image", shown)

		cmd := exec.CommandContext(ctx, runtimeBin, "image", "pull", image)
		cmd.Stdout = logger
		cmd.Stderr = logger
		if err := cmd.Run(); err != nil {
			_, _ = fmt.Fprintf(logger, "  ERROR: %v\n\n", err)
			log.Error("Image pull failed", "image", shown, "error", err)
			lastErr = err
			continue
		}
		_, _ = fmt.Fprintf(logger, "  OK\n\n")
		log.Debug("Image pulled successfully", "image", shown)
		pulled++
	}

	_, _ = fmt.Fprintf(logger, "Pull complete: %d/%d succeeded\n", pulled, len(harnesses))
	if lastErr != nil && pulled == 0 {
		return fmt.Errorf("all image pulls failed; last error: %w", lastErr)
	}
	log.Info("Pull images complete", "pulled", pulled, "total", len(harnesses))
	return nil
}

// RebuildServerExecutor rebuilds the hub binary from the maintenance repo's
// Git checkout and installs it through the host's Installer, like a
// release:
//
//  1. Installer.Preflight, before anything is fetched or built;
//  2. git fetch, checkout/reset to origin/<branch> (or the current branch);
//  3. make web, then go build with the commit and build time stamped in
//     (GitBuildLDFlags) into a temporary directory, and VerifyGitBuild;
//  4. Installer.Stage under the release name git-<sha12> (on the symlink
//     layout releases/git-<sha12>/, swapped and retained like a release;
//     with sudo-install <RepoPath>/scion.rebuild, installed with the
//     starter hub's sudoers rules), Installer.Activate;
//  5. on a starter hub (sudo-install), the systemd unit drift check;
//  6. the before-restart hook, then Installer.Restart. If either fails an
//     installer that implements Deactivator is rolled back.
//
// Boot reconciliation verifies the run by the restarted hub's release name
// (RunningReleaseName) and then records the Git selection.
type RebuildServerExecutor struct {
	repoPath    string // path to scion source checkout
	repoBranch  string // git branch to checkout before building (empty = stay on current)
	serviceName string // systemd service name, for the unit drift check
	installer   Installer
	current     string        // RunningReleaseName of the running build
	releases    ReleaseSource // dates a preview/stable current; nil skips the lookup
	snapshots   DBSnapshotter // the pre-update database snapshot (design D4)

	// unitLocator and unitRevision back the systemd unit drift check;
	// tests replace them (RebuildServerOptions) so they never call the
	// host's systemctl or git.
	unitLocator  unitdrift.Locator
	unitRevision unitdrift.Revision
	unitTimeout  time.Duration // zero means unitdrift.DefaultTimeout

	// store, when set, keeps the last unit check result for the
	// Admin -> Maintenance banner (hub setting system.unit_drift).
	store store.Store

	// settings lists the required settings that are unset; nil skips the
	// settings check section.
	settings func() []preflight.Finding

	preflightTimeout time.Duration
	installTimeout   time.Duration
	hookTimeout      time.Duration
	restartTimeout   time.Duration
}

// RebuildServerOptions configures a RebuildServerExecutor.
type RebuildServerOptions struct {
	RepoPath    string // path to scion source checkout
	RepoBranch  string // git branch to checkout before building (empty = stay on current)
	ServiceName string // systemd service name for the unit drift check; default DefaultServiceName
	// Installer installs and restarts (the one detected at startup). nil
	// means unsupported: the run fails at preflight.
	Installer Installer
	// CurrentRelease is the running build's RunningReleaseName (the
	// pending update's "from"); default from pkg/version.
	CurrentRelease string
	// Releases looks up the publish date of a preview or stable
	// CurrentRelease, for the boot-failure guard's previous_safe (as
	// update-binary does). nil skips the lookup: such a "from" is then not
	// safe to roll back to after migrations (fail closed).
	Releases ReleaseSource
	// Snapshotter takes the pre-update database snapshot, as for
	// update-binary (design D4: before every update). nil means snapshots
	// are unsupported: the rebuild goes ahead without one, as an upgrade
	// does.
	Snapshotter DBSnapshotter
	// Store, when set, keeps the last systemd unit check result for the
	// Admin -> Maintenance page. Optional.
	Store store.Store
	// UnitLocator and UnitRevision back the systemd unit drift check; nil
	// means unitdrift.SystemctlLocator and unitdrift.GitRevision. Tests set
	// them so they never call the host's systemctl or git.
	UnitLocator  unitdrift.Locator
	UnitRevision unitdrift.Revision
	// UnitTimeout bounds the unit check; zero means unitdrift.DefaultTimeout.
	UnitTimeout time.Duration
	// SettingsCheck, when set, lists the required settings that are unset
	// (preflight.LoadRequiredSettingsFindings); Run writes them to the log
	// as warnings and goes on. nil skips the section.
	SettingsCheck func() []preflight.Finding
}

// NewRebuildServerExecutor returns a RebuildServerExecutor configured from opts.
func NewRebuildServerExecutor(opts RebuildServerOptions) *RebuildServerExecutor {
	e := &RebuildServerExecutor{
		repoPath:         opts.RepoPath,
		repoBranch:       opts.RepoBranch,
		serviceName:      opts.ServiceName,
		installer:        opts.Installer,
		current:          opts.CurrentRelease,
		releases:         opts.Releases,
		snapshots:        opts.Snapshotter,
		unitLocator:      unitdrift.SystemctlLocator,
		unitRevision:     unitdrift.GitRevision,
		unitTimeout:      opts.UnitTimeout,
		store:            opts.Store,
		settings:         opts.SettingsCheck,
		preflightTimeout: preflightTimeout,
		installTimeout:   installTimeout,
		hookTimeout:      hookTimeout,
		restartTimeout:   restartTimeout,
	}
	if e.serviceName == "" {
		e.serviceName = DefaultServiceName
	}
	if e.installer == nil {
		e.installer = NewUnsupportedInstaller("no installer configured", "")
	}
	if e.current == "" {
		e.current = RunningReleaseName(version.Version, version.GetCommit())
	}
	if e.snapshots == nil {
		e.snapshots = NewUnsupportedDBSnapshotter("no database snapshotter is configured")
	}
	if opts.UnitLocator != nil {
		e.unitLocator = opts.UnitLocator
	}
	if opts.UnitRevision != nil {
		e.unitRevision = opts.UnitRevision
	}
	return e
}

// Run implements MaintenanceExecutor; runners that record runs use
// RunWithBeforeRestart.
func (e *RebuildServerExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	return e.RunWithBeforeRestart(ctx, logger, params, nil)
}

// rebuildStep is one command of a rebuild.
type rebuildStep struct {
	name string
	cmd  string
	args []string
	dir  string
}

// RunWithBeforeRestart implements RestartingExecutor.
func (e *RebuildServerExecutor) RunWithBeforeRestart(ctx context.Context, logger io.Writer, params map[string]string, before BeforeRestartFunc) error {
	log := logging.Subsystem("hub.maintenance.rebuild-server")

	if runtime.GOOS != "linux" {
		return fmt.Errorf("rebuild-server is only supported on Linux (requires systemd); restart the server manually on %s", runtime.GOOS)
	}

	repoPath := e.repoPath
	if repoPath == "" {
		return fmt.Errorf("no repository path configured for rebuild-server")
	}

	// A "branch" param on the API request overrides the configured default.
	branch := e.repoBranch
	if v := params["branch"]; v != "" {
		branch = v
	}

	log.Debug("Starting rebuild-server",
		"repo_path", repoPath, "branch", branch,
		"installer", e.installer.Name(), "service_name", e.serviceName)

	// ── Preflight: before anything is fetched or built ──────────────────
	_, _ = fmt.Fprintf(logger, "==> Preflight (installer: %s)\n", e.installer.Name())
	preCtx, cancelPre := context.WithTimeout(ctx, e.preflightTimeout)
	err := e.installer.Preflight(preCtx)
	cancelPre()
	if err != nil {
		return fmt.Errorf("installer preflight failed (%s): %w", e.installer.Name(), err)
	}
	_, _ = fmt.Fprintf(logger, "Preflight passed.\n\n")

	// Warnings only: an unset required setting never stops the rebuild.
	// After the installer preflight, as for update-binary.
	if e.settings != nil {
		preflight.WriteSettingsCheck(logger, e.settings())
	}

	// ── Update the checkout ─────────────────────────────────────────────
	plan := rebuildPlan{repoPath: repoPath, branch: branch}
	if branch == "" {
		// Determine the current branch to reset against its remote tracking ref.
		out, err := gitOutput(ctx, repoPath, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return fmt.Errorf("failed to determine current branch: %w", err)
		}
		plan.currentBranch = out
	}
	if err := runRebuildSteps(ctx, logger, rebuildCheckout, plan); err != nil {
		return err
	}

	commit, err := gitOutput(ctx, repoPath, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("failed to read the checked-out commit: %w", err)
	}
	name := GitReleaseName(commit)
	if name == "" {
		return fmt.Errorf("git rev-parse HEAD returned %q, not a commit id", commit)
	}
	_, _ = fmt.Fprintf(logger, "Building commit %s as %s\n\n", commit, name)
	for _, r := range e.installer.Retained() {
		if r.Current && r.Tag == name {
			return fmt.Errorf("%s is already the active build (commit %s); there is nothing new to build", name, commit)
		}
	}

	// ── Build into a temporary directory and verify ─────────────────────
	tmpDir, err := os.MkdirTemp("", "scion-rebuild-*")
	if err != nil {
		return fmt.Errorf("create temp directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	plan.bin = filepath.Join(tmpDir, "scion")
	plan.ldflags = GitBuildLDFlags(commit, time.Now())
	if err := runRebuildSteps(ctx, logger, rebuildBuild, plan); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(logger, "==> Verifying the build\n")
	if err := VerifyGitBuild(ctx, plan.bin, commit, logger); err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}

	// ── Database snapshot, before anything changes ──────────────────────
	// As for update-binary (design D4). It is what "Restoring after a
	// failed update" restores, including after a Git build the
	// boot-failure guard leaves stopped. On the run ctx: VACUUM INTO of a
	// large database can take a while.
	snapshot, err := takeUpdateSnapshot(ctx, e.snapshots, e.current, logger, "hub.maintenance.rebuild-server",
		"The rebuild goes ahead without one.")
	if err != nil {
		return err
	}

	// The boot-failure guard may roll back to the running release only if
	// it is at or above the new build's downgrade floor; a preview or
	// stable "from" needs its publish date for that (update-binary does the
	// same). A Git "from" stays undated: not provably safe, fail closed.
	lookupCtx, cancelLookup := context.WithTimeout(ctx, releaseAPITimeout)
	fromPublished := releasePublishedAt(lookupCtx, e.releases, e.current)
	cancelLookup()

	// ── Install ─────────────────────────────────────────────────────────
	_, _ = fmt.Fprintf(logger, "\n==> Installing %s (%s)\n", name, e.installer.Name())
	installCtx, cancelInstall := context.WithTimeout(ctx, e.installTimeout)
	staged, err := e.installer.Stage(installCtx, name, plan.bin)
	if err != nil {
		cancelInstall()
		return fmt.Errorf("stage %s: %w", name, err)
	}
	err = e.installer.Activate(installCtx, staged)
	cancelInstall()
	if err != nil {
		return fmt.Errorf("activate %s: %w", name, err)
	}
	_, _ = fmt.Fprintf(logger, "Installed %s.\n\n", name)

	// ── Systemd unit check (starter hubs) ───────────────────────────────
	// The template it compares against is the starter hub's
	// (unitdrift.TemplatePath); a deploy.sh hub (symlink layout) renders
	// its own unit, so the check applies to sudo-install only. Rebuild
	// from git never rewrites the unit, so a template change needs a
	// re-provision; tell the admin. The check is read-only, bounded and
	// never fails, so the restart below always runs. Its report is the
	// last part of the log written before the restart is recorded.
	var unitText string
	if e.installer.Name() == StrategySudoInstall {
		unitText = e.checkUnit(ctx, logger, repoPath, branch, plan.currentBranch)
	} else {
		// Not a starter-hub unit: a drift record left from before (the
		// Maintenance banner) can no longer be refreshed, so it is removed,
		// as for a structural skip.
		e.persistUnitStatus(ctx, logger, unitdrift.Report{Status: unitdrift.Skipped, Structural: true}, "", e.serviceName)
	}

	// ── Restart ─────────────────────────────────────────────────────────
	_, _ = fmt.Fprintf(logger, "==> Restarting hub: %s -> %s at %s\n",
		displayVersion(e.current), name, time.Now().UTC().Format(time.RFC3339))
	_, _ = fmt.Fprintln(logger, "Server rebuild installed; requesting the restart.")
	if unitText != "" {
		_, _ = fmt.Fprintf(logger, "\n==> Systemd unit check\n%s", unitText)
	}
	if before != nil {
		p := PendingRestart{
			From:     e.current,
			To:       name,
			Strategy: e.installer.Name(),
			Snapshot: snapshot,
			Mode:     UpdateModeGit,
			Commit:   commit,
			// For the boot-failure guard's previous_safe.
			FromPublishedAt: fromPublished,
		}
		hookCtx, cancelHook := context.WithTimeout(ctx, e.hookTimeout)
		err := before(hookCtx, p)
		cancelHook()
		if err != nil {
			log.Error("Before-restart hook failed; not restarting", "error", err)
			return undoInstall(ctx, e.installer, e.installTimeout, logger, name, nil,
				fmt.Errorf("recording the restart failed, so the hub was not restarted: %w", err))
		}
	}
	restartCtx, cancelRestart := context.WithTimeout(ctx, e.restartTimeout)
	err = e.installer.Restart(restartCtx)
	cancelRestart()
	if err != nil {
		log.Error("Failed to restart the hub", "error", err)
		return undoInstall(ctx, e.installer, e.installTimeout, logger, name, nil,
			fmt.Errorf("restarting the hub failed: %w", err))
	}
	log.Info("Server rebuild installed, restart requested", "from", e.current, "to", name)
	return nil
}

// checkUnit compares the installed systemd unit with the template in the
// tree just checked out, records the result for the Maintenance banner
// and returns the report text.
func (e *RebuildServerExecutor) checkUnit(ctx context.Context, logger io.Writer, repoPath, branch, currentBranch string) string {
	_, _ = fmt.Fprintf(logger, "==> Checking systemd unit against template (result at end of log)\n\n")
	unitBranch := branch
	if unitBranch == "" {
		unitBranch = currentBranch
	}
	if unitBranch == "HEAD" { // detached checkout: no branch to name
		unitBranch = ""
	}
	rep := unitdrift.Check(ctx, unitdrift.CheckOptions{
		RepoPath: repoPath,
		Branch:   unitBranch,
		Service:  e.serviceName,
		Locate:   e.unitLocator,
		Revision: e.unitRevision,
		Timeout:  e.unitTimeout,
	})
	if rep.Status == unitdrift.Drift {
		logging.Subsystem("hub.maintenance.rebuild-server").Warn(
			"Installed systemd unit differs from template; re-provision needed", "service_name", e.serviceName)
	}
	// Persisted now, before the restart, so the restart cannot lose it.
	e.persistUnitStatus(ctx, logger, rep, unitBranch, e.serviceName)
	return rep.Text
}

// rebuildPhase selects the step table runRebuildSteps runs.
type rebuildPhase int

const (
	// rebuildCheckout updates the checkout: git fetch, then checkout and
	// reset to origin/<branch> (or reset the current branch).
	rebuildCheckout rebuildPhase = iota
	// rebuildBuild builds the web assets and the binary.
	rebuildBuild
)

// rebuildPlan carries what the step tables need.
type rebuildPlan struct {
	repoPath      string
	branch        string // configured or requested branch; "" = currentBranch
	currentBranch string // the checkout's branch, when branch is ""
	bin           string // build output (rebuildBuild)
	ldflags       string // -ldflags value (rebuildBuild)
}

// runRebuildSteps builds the step table of phase and runs it in order,
// logging each step, and stops at the first failure. The table (its git
// commands included) and the process start are kept in this one function so
// the host-git door guard (pkg/util TestHostGitGoesThroughOneDoor) counts
// them together.
func runRebuildSteps(ctx context.Context, logger io.Writer, phase rebuildPhase, p rebuildPlan) error {
	var steps []rebuildStep
	switch phase {
	case rebuildCheckout:
		steps = append(steps, rebuildStep{"Fetching latest code", "git", []string{"fetch", "origin"}, p.repoPath})
		if p.branch != "" {
			steps = append(steps, rebuildStep{"Checking out branch " + p.branch, "git", []string{"checkout", p.branch}, p.repoPath})
			steps = append(steps, rebuildStep{"Resetting to origin/" + p.branch, "git", []string{"reset", "--hard", "origin/" + p.branch}, p.repoPath})
		} else {
			steps = append(steps, rebuildStep{"Resetting to origin/" + p.currentBranch, "git", []string{"reset", "--hard", "origin/" + p.currentBranch}, p.repoPath})
		}
	case rebuildBuild:
		steps = append(steps,
			rebuildStep{"Building web assets", "make", []string{"web"}, p.repoPath},
			rebuildStep{"Building server binary", "go", []string{"build", "-ldflags", p.ldflags, "-o", p.bin, "./cmd/scion"}, p.repoPath},
		)
	default:
		return fmt.Errorf("unknown rebuild phase %d", phase)
	}
	log := logging.Subsystem("hub.maintenance.rebuild-server")
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
	return nil
}

// gitOutput runs git in dir and returns its trimmed stdout.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), detail)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// UnitDriftStatus is the stored result of the last systemd unit check,
// shown on Admin -> Maintenance. Only "match" and "drift" are stored.
type UnitDriftStatus struct {
	Status    string   `json:"status"` // "match" or "drift"
	CheckedAt string   `json:"checkedAt"`
	Commit    string   `json:"commit"`
	Branch    string   `json:"branch"`
	Service   string   `json:"service"` // systemd unit name, e.g. scion-hub.service
	Summary   []string `json:"summary,omitempty"`
}

// unitStatusTimeout bounds the store call that records the unit check.
const unitStatusTimeout = 5 * time.Second

// persistUnitStatus records the unit check for the Maintenance banner:
// match and drift are stored, a structural skip removes the record (the
// host is no longer comparable, so an old drift must not stick), and a
// transient skip leaves it as it is. It never fails the run and recovers
// from panics, so the restart that follows always runs.
func (e *RebuildServerExecutor) persistUnitStatus(ctx context.Context, logger io.Writer, rep unitdrift.Report, branch, service string) {
	if e.store == nil {
		return
	}
	log := logging.Subsystem("hub.maintenance.rebuild-server")
	defer func() {
		if r := recover(); r != nil {
			log.Warn("Recording the systemd unit status panicked", "panic", fmt.Sprint(r))
			_, _ = fmt.Fprintf(logger, "Warning: could not record the systemd unit status: internal error\n\n")
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, unitStatusTimeout)
	defer cancel()

	var status string
	switch rep.Status {
	case unitdrift.Match:
		status = "match"
	case unitdrift.Drift:
		status = "drift"
	default:
		if !rep.Structural {
			return
		}
		if err := e.store.DeleteHubSetting(ctx, HubSettingSectionUnitDrift); err != nil && !errors.Is(err, store.ErrNotFound) {
			log.Warn("Failed to clear the systemd unit status", "error", err)
			_, _ = fmt.Fprintf(logger, "Warning: could not clear the systemd unit status: %v\n\n", err)
			return
		}
		_, _ = fmt.Fprintf(logger, "Cleared the recorded systemd unit status (check not applicable).\n\n")
		return
	}
	value, err := json.Marshal(UnitDriftStatus{
		Status:    status,
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
		Commit:    rep.Commit,
		Branch:    branch,
		Service:   unitdrift.UnitName(service),
		Summary:   rep.Diff, // already elided by unitdrift
	})
	if err != nil {
		log.Warn("Failed to encode the systemd unit status", "error", err)
		return
	}
	if _, err := e.store.UpsertHubSetting(ctx, HubSettingSectionUnitDrift, json.RawMessage(value), "system", -1, "managed"); err != nil {
		log.Warn("Failed to record the systemd unit status", "error", err)
		_, _ = fmt.Fprintf(logger, "Warning: could not record the systemd unit status: %v\n\n", err)
		return
	}
	_, _ = fmt.Fprintf(logger, "Recorded systemd unit status: %s\n\n", status)
}

// RebuildWebExecutor rebuilds the web frontend assets from source.
type RebuildWebExecutor struct {
	repoPath   string // path to scion source checkout
	repoBranch string // git branch to checkout before building (empty = stay on current)
}

// RebuildWebOptions configures a RebuildWebExecutor. Zero values are kept
// as-is; Run applies its own checks.
type RebuildWebOptions struct {
	RepoPath   string // path to scion source checkout
	RepoBranch string // git branch to checkout before building (empty = stay on current)
}

// NewRebuildWebExecutor returns a RebuildWebExecutor configured from opts.
func NewRebuildWebExecutor(opts RebuildWebOptions) *RebuildWebExecutor {
	return &RebuildWebExecutor{
		repoPath:   opts.RepoPath,
		repoBranch: opts.RepoBranch,
	}
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

// NewRebuildContainerBinariesExecutor returns a
// RebuildContainerBinariesExecutor that builds from the checkout at repoPath.
func NewRebuildContainerBinariesExecutor(repoPath string) *RebuildContainerBinariesExecutor {
	return &RebuildContainerBinariesExecutor{
		repoPath: repoPath,
	}
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
	objectPath func(hc *store.HarnessConfig, f store.TemplateFile) string
}

// BuildHarnessConfigImageOptions configures a BuildHarnessConfigImageExecutor.
// Zero values are kept as-is; Run applies its own defaults.
type BuildHarnessConfigImageOptions struct {
	Store      store.Store
	Storage    storage.Storage
	RuntimeBin string
	Registry   string
	Tag        string
	GCPProject string
	// ObjectPath returns the storage object path of a harness-config file.
	// Required: the executors call it for every file they materialize.
	ObjectPath func(hc *store.HarnessConfig, f store.TemplateFile) string
}

// NewBuildHarnessConfigImageExecutor returns a BuildHarnessConfigImageExecutor
// configured from opts.
func NewBuildHarnessConfigImageExecutor(opts BuildHarnessConfigImageOptions) *BuildHarnessConfigImageExecutor {
	return &BuildHarnessConfigImageExecutor{
		store:      opts.Store,
		storage:    opts.Storage,
		runtimeBin: opts.RuntimeBin,
		registry:   opts.Registry,
		tag:        opts.Tag,
		gcpProject: opts.GCPProject,
		objectPath: opts.ObjectPath,
	}
}

func (e *BuildHarnessConfigImageExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	if params["builder"] == "cloud-build" {
		cloudExecutor := &CloudBuildHarnessConfigExecutor{
			store:      e.store,
			storage:    e.storage,
			gcpProject: e.gcpProject,
			registry:   e.registry,
			tag:        e.tag,
			objectPath: e.objectPath,
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

	if err := materializeHarnessConfigFiles(ctx, e.storage, e.objectPath, hc, tmpDir, logger); err != nil {
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

	imageName := DeriveImageBaseName(hc)
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

	// Record the newly-built image on the harness config (BuiltImage), so
	// the image status display and checks show it.
	if err := SyncBuiltImage(ctx, e.store, logger, hc, outputImage); err != nil {
		log.Error("Failed to sync built image back to store", "error", err)
		_, _ = fmt.Fprintf(logger, "Warning: build succeeded but failed to update harness-config image: %v\n", err)
	}

	_, _ = fmt.Fprintf(logger, "\nBuild complete: %s\n", outputImage)
	log.Info("Build complete", "image", outputImage, "harness_config", hc.Name)
	return nil
}

// materializeHarnessConfigFiles downloads all files of a harness config (its
// blobs, or its path objects for a legacy row) into the given directory. It
// is shared by both the local and Cloud Build executors. objectPath gives
// each file's storage object path.
func materializeHarnessConfigFiles(ctx context.Context, stor storage.Storage, objectPath func(hc *store.HarnessConfig, f store.TemplateFile) string, hc *store.HarnessConfig, destDir string, logger io.Writer) error {
	_, _ = fmt.Fprintf(logger, "Materializing %d file(s) from harness-config %q...\n", len(hc.Files), hc.Name)
	for _, f := range hc.Files {
		reader, _, err := stor.Download(ctx, objectPath(hc, f))
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
	objectPath func(hc *store.HarnessConfig, f store.TemplateFile) string
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

	if err := materializeHarnessConfigFiles(ctx, e.storage, e.objectPath, hc, tmpDir, logger); err != nil {
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

	imageName := DeriveImageBaseName(hc)
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

			if err := SyncBuiltImage(ctx, e.store, logger, hc, outputImage); err != nil {
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

// DeriveImageBaseName returns the base image name (without tag) for a
// harness config, preferring the config.yaml image field and falling
// back to the slug or name.
func DeriveImageBaseName(hc *store.HarnessConfig) string {
	if hc.Config != nil && hc.Config.Image != "" {
		name := hc.Config.Image
		// Only strip a tag — the colon must appear after the last slash
		// to distinguish "registry:port/image" from "image:tag".
		if idx := strings.LastIndex(name, ":"); idx >= 0 {
			slashIdx := strings.LastIndex(name, "/")
			if idx > slashIdx {
				name = name[:idx]
			}
		}
		return name
	}
	if hc.Slug != "" {
		return hc.Slug
	}
	return hc.Name
}

// SyncBuiltImage records the newly-built image on the harness config's DB
// record without modifying config.yaml in storage. It writes only the
// BuiltImage column (store.UpdateHarnessConfigBuiltImage): the built image
// is operational state, not derived from config.yaml, so it never writes
// Config.Image, which the commit path derives and would otherwise clear on
// the next commit (ptone/scion#4219). The image handlers show and check
// BuiltImage when set (effectiveHarnessConfigImage). The image field in
// config.yaml stays a stable reference; two-phase resolution at agent start
// prefers local images. This matches the CLI build path's behavior (see
// cmd/build.go syncBuildToHub).
func SyncBuiltImage(ctx context.Context, storeDB store.Store, logger io.Writer, hc *store.HarnessConfig, outputImage string) error {
	if err := storeDB.UpdateHarnessConfigBuiltImage(ctx, hc.ID, outputImage); err != nil {
		return fmt.Errorf("failed to update harness-config record: %w", err)
	}
	hc.BuiltImage = outputImage
	_, _ = fmt.Fprintf(logger, "Updated harness-config record built image to %s\n", outputImage)

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

// UpdateCheckResult holds the outcome of a source-based update check.
type UpdateCheckResult struct {
	UpdateAvailable bool `json:"update_available"`
	// CurrentCommit is the commit compared with the tracking ref: the
	// running build's commit when the checkout has it, else the checkout's
	// HEAD (see Compared).
	CurrentCommit string `json:"current_commit"`
	// Compared is UpdateCheckRunning or UpdateCheckCheckout.
	Compared      string             `json:"compared"`
	LatestCommit  string             `json:"latest_commit"`
	CurrentBranch string             `json:"current_branch"`
	TrackingRef   string             `json:"tracking_ref"`
	CommitsBehind int                `json:"commits_behind"`
	NewCommits    []UpdateCommitInfo `json:"new_commits,omitempty"`
}

// What a source update check compared with the tracking ref.
const (
	// UpdateCheckRunning: the running build's commit.
	UpdateCheckRunning = "running"
	// UpdateCheckCheckout: the maintenance checkout's HEAD (the running
	// commit is unknown or not in the checkout).
	UpdateCheckCheckout = "checkout"
)

// UpdateCommitInfo describes a single commit available in the remote.
type UpdateCommitInfo struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
}

// UpdateCheckOptions configures CheckForUpdates.
type UpdateCheckOptions struct {
	// RepoPath is the maintenance checkout.
	RepoPath string
	// Branch is the configured branch Rebuild builds; "" means the
	// checkout's current branch (as Rebuild does).
	Branch string
	// RunningCommit is the running build's commit. When the checkout has
	// it, it is what is compared, so "up to date" means the running hub
	// (not merely the checkout) has the branch's commits.
	RunningCommit string
}

// CheckForUpdates fetches origin in the maintenance checkout and compares
// the running commit (else the checkout's HEAD) with origin/<branch>.
func CheckForUpdates(ctx context.Context, opts UpdateCheckOptions) (*UpdateCheckResult, error) {
	log := logging.Subsystem("hub.maintenance.check-updates")

	repoPath := opts.RepoPath
	if repoPath == "" {
		return nil, fmt.Errorf("no repository path configured")
	}

	log.Debug("Checking for updates", "repo_path", repoPath, "branch", opts.Branch)

	// The fetch output can name the remote (with any credentials in its
	// URL): it goes to the server log only, never into the API error.
	fetchCmd := exec.CommandContext(ctx, "git", "fetch", "origin")
	fetchCmd.Dir = repoPath
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		log.Error("git fetch failed", "error", err, "output", string(out))
		return nil, fmt.Errorf("git fetch failed: %w", err)
	}

	currentBranch := opts.Branch
	if currentBranch == "" {
		b, err := gitOutput(ctx, repoPath, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("failed to get current branch: %w", err)
		}
		currentBranch = b
	}
	trackingRef := "origin/" + currentBranch

	base, compared := "", UpdateCheckCheckout
	// Only a hex commit id reaches git (never an option-like value), and
	// only when the checkout has that commit.
	if GitReleaseName(opts.RunningCommit) != "" {
		running := strings.ToLower(strings.TrimSpace(opts.RunningCommit))
		if full, err := gitOutput(ctx, repoPath, "rev-parse", "--verify", "--quiet", running+"^{commit}"); err == nil {
			base, compared = full, UpdateCheckRunning
		}
	}
	if base == "" {
		head, err := gitOutput(ctx, repoPath, "rev-parse", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("failed to get local HEAD: %w", err)
		}
		base = head
	}

	remoteCommit, err := gitOutput(ctx, repoPath, "rev-parse", "--verify", trackingRef+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("failed to get %s: %w", trackingRef, err)
	}

	result := &UpdateCheckResult{
		CurrentCommit: base,
		Compared:      compared,
		LatestCommit:  remoteCommit,
		CurrentBranch: currentBranch,
		TrackingRef:   trackingRef,
	}
	if base == remoteCommit {
		return result, nil
	}

	// Commits on the tracking ref that the compared commit lacks.
	logOut, err := gitOutput(ctx, repoPath, "log", "--oneline", "--no-decorate", base+".."+remoteCommit)
	if err != nil {
		// Non-fatal: the commits differ, but they cannot be listed.
		log.Warn("Failed to list new commits", "error", err)
		result.UpdateAvailable = true
		return result, nil
	}
	for _, line := range strings.Split(logOut, "\n") {
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
	// A compared commit ahead of the tracking ref (none missing) is not
	// behind: no update.
	result.UpdateAvailable = result.CommitsBehind > 0

	log.Info("Update check complete",
		"update_available", result.UpdateAvailable,
		"commits_behind", result.CommitsBehind,
		"branch", currentBranch,
		"tracking_ref", trackingRef,
		"compared", compared,
		"current", base,
		"latest", remoteCommit)

	return result, nil
}

// DefaultMaxExtractBytes caps the decompressed size of a release tarball
// read by ExtractScionBinary. It matches DefaultMaxDownloadBytes (512 MiB):
// a release archive holds one scion binary of a few hundred MB at most, so
// anything larger is refused instead of being inflated (gzip bomb).
const DefaultMaxExtractBytes int64 = 512 << 20

// ExtractScionBinary extracts the scion binary from a release .tar.gz into
// destDir/extracted and returns its path. Release archives
// (build-release.yml: tar -czf … -C dist scion) hold exactly one regular
// file named "scion" at the archive root; that is the only accepted layout:
//   - a second root-level scion entry, a scion entry in a subdirectory, or a
//     root-level scion that is not a regular file is an error;
//   - other entries are skipped, never written to disk;
//   - a sparse scion entry is an error (its holes are not read from input);
//   - reading stops with an error once more than DefaultMaxExtractBytes have
//     been decompressed, a scion entry declaring more than that is refused
//     before writing, and the written binary is bounded to it as well.
func ExtractScionBinary(tarballPath, destDir string, logger io.Writer) (string, error) {
	return extractScionBinary(tarballPath, destDir, logger, DefaultMaxExtractBytes)
}

// errExtractTooLarge reports a tarball that decompresses past the cap.
var errExtractTooLarge = errors.New("release archive exceeds the extraction size limit")

// cappedReader returns errExtractTooLarge once more than n bytes were read.
type cappedReader struct {
	r io.Reader
	n int64 // bytes still allowed
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n < 0 {
		return 0, errExtractTooLarge
	}
	if int64(len(p)) > c.n+1 {
		p = p[:c.n+1]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	if c.n < 0 {
		return n, errExtractTooLarge
	}
	return n, err
}

func extractScionBinary(tarballPath, destDir string, logger io.Writer, maxBytes int64) (string, error) {
	return extractRootEntry(tarballPath, destDir, "scion", logger, maxBytes)
}

// extractRootEntry extracts the single root-level regular file entry of a
// release .tar.gz into destDir/extracted/<entry> and returns its path, with
// the rules and caps of ExtractScionBinary. Release plugin archives
// (build-release.yml: tar -czvf … -C dist scion-plugin-<name>) have the
// same layout with entry scion-plugin-<name>.
func extractRootEntry(tarballPath, destDir, entry string, logger io.Writer, maxBytes int64) (string, error) {
	f, err := os.Open(tarballPath)
	if err != nil {
		return "", fmt.Errorf("open tarball: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("create gzip reader: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(&cappedReader{r: gz, n: maxBytes})
	extractDir := filepath.Join(destDir, "extracted")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return "", fmt.Errorf("create extract directory: %w", err)
	}
	binPath := filepath.Join(extractDir, entry)

	found := false
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, errExtractTooLarge) {
				return "", fmt.Errorf("%w (%d bytes)", errExtractTooLarge, maxBytes)
			}
			return "", fmt.Errorf("read tarball entry: %w", err)
		}

		name := path.Clean(strings.TrimPrefix(header.Name, "./"))
		switch {
		case name == entry:
			if found {
				return "", fmt.Errorf("release archive has more than one %q entry", entry)
			}
			if header.Typeflag != tar.TypeReg {
				return "", fmt.Errorf("release archive entry %q is not a regular file", header.Name)
			}
			// A sparse entry's holes are synthesized as zeros without
			// reading input, so the stream cap does not bound it. Release
			// binaries are never sparse.
			if isSparseEntry(header) {
				return "", fmt.Errorf("release archive entry %q is a sparse file; refusing it", header.Name)
			}
			if header.Size > maxBytes {
				return "", fmt.Errorf("%w: entry %q declares %d bytes (limit %d)", errExtractTooLarge, header.Name, header.Size, maxBytes)
			}
			found = true
			if err := writeExtracted(binPath, tr, maxBytes); err != nil {
				if errors.Is(err, errExtractTooLarge) {
					return "", fmt.Errorf("%w (%d bytes)", errExtractTooLarge, maxBytes)
				}
				return "", err
			}
			_, _ = fmt.Fprintf(logger, "Found %s binary: %s\n", entry, header.Name)
		case path.Base(name) == entry:
			return "", fmt.Errorf("release archive has a nested %q entry (%s); only a root-level %s is accepted", entry, header.Name, entry)
		default:
			// Not extracted. Its content is still read (and counted against
			// the cap) by the next tr.Next.
		}
	}

	if !found {
		return "", fmt.Errorf("no '%s' binary found at the root of the tarball", entry)
	}
	return binPath, nil
}

// isSparseEntry reports whether hdr describes a GNU sparse file, in either
// the old GNU format or the PAX GNU.sparse.* encoding (which archive/tar
// presents as a regular file).
func isSparseEntry(hdr *tar.Header) bool {
	if hdr.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
}

// writeExtracted writes r to dst with mode 0755, at most maxBytes: the
// output is bounded on its own, independently of the input-side cap.
func writeExtracted(dst string, r io.Reader, maxBytes int64) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	n, err := io.CopyN(out, r, maxBytes+1)
	if err != nil && err != io.EOF {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("write %s: %w", dst, err)
	}
	if n > maxBytes {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("%w: %s exceeds %d bytes", errExtractTooLarge, dst, maxBytes)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return os.Chmod(dst, 0o755)
}

// VerifyScionBinary runs the extracted binary to confirm it reports the
// expected version. This doubles as an integrity check — a corrupt or
// truncated download will fail to execute.
func VerifyScionBinary(ctx context.Context, binaryPath, expectedVersion string, logger io.Writer) error {
	cmd := exec.CommandContext(ctx, binaryPath, "version", "--format", "json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	_, _ = fmt.Fprintf(logger, "Running: %s version --format json\n", binaryPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("binary version check failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}

	var versionOutput struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &versionOutput); err != nil {
		return fmt.Errorf("failed to parse version output: %w (raw: %s)", err, strings.TrimSpace(stdout.String()))
	}

	if versionOutput.Version != expectedVersion {
		return fmt.Errorf("version mismatch: binary reports %q, expected %q", versionOutput.Version, expectedVersion)
	}

	_, _ = fmt.Fprintf(logger, "Version verified: %s\n", versionOutput.Version)
	return nil
}

// ParseMigrationParams extracts and validates migration-specific parameters from the request body.
func ParseMigrationParams(body map[string]interface{}) map[string]string {
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
