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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/GoogleCloudPlatform/scion/pkg/version/update"
)

const (
	// DefaultGitHubRepo is the release repository when none is configured.
	DefaultGitHubRepo = "GoogleCloudPlatform/scion"

	// DefaultMaxDownloadBytes caps the release tarball download.
	DefaultMaxDownloadBytes int64 = 512 << 20

	// DefaultDownloadTimeout bounds the whole resolve + download + verify
	// phase of an update.
	DefaultDownloadTimeout = 10 * time.Minute

	// LatestTargetPrefix marks a target_version of the form latest:<channel>.
	LatestTargetPrefix = "latest:"
)

// update-binary parameters.
const (
	ParamTargetVersion = "target_version"
	ParamChannel       = "channel"
	ParamPin           = "pin"
	// ParamConfirmDowngrade must equal the resolved target tag to install
	// an older release (a downgrade or switch-older; design D4).
	ParamConfirmDowngrade = "confirm_downgrade"
)

// PinLatest is the pin of a hub that follows its channel's latest release
// (and may be auto-updated); any other pin is a release tag.
const PinLatest = "latest"

// UpdateBinaryParams is the complete set of update-binary parameters. Any
// other parameter is refused with CodeUnknownParam.
var UpdateBinaryParams = []string{ParamTargetVersion, ParamChannel, ParamPin, ParamConfirmDowngrade}

// Request error codes returned (as *RequestError) by ValidateParams and Run.
const (
	CodeUnknownParam   = "unknown_param"
	CodeInvalidParam   = "invalid_param"
	CodeAlreadyRunning = "already_running"
	// CodeDowngradeRequiresConfirmation refuses an older target (downgrade
	// or switch-older) whose confirm_downgrade parameter is missing or is
	// not the resolved target tag. Its Details carry the risk list (see
	// DowngradeDetails).
	CodeDowngradeRequiresConfirmation = "downgrade_requires_confirmation"
	// CodeBelowDowngradeFloor refuses a target older than the running
	// binary's version.DowngradeFloor, whether confirmed or not.
	CodeBelowDowngradeFloor = "below_downgrade_floor"
	// CodeDowngradeUnsupportedDriver refuses an older target on a hub whose
	// database cannot be snapshotted (PostgreSQL).
	CodeDowngradeUnsupportedDriver = "downgrade_unsupported_driver"
)

// RequestError is a refusal of the requested operation that an API handler
// reports with a specific error code (see HTTPStatus).
type RequestError struct {
	Code    string
	Message string
	// Details is extra machine-readable context for the API error's
	// "details" object (nil for none).
	Details map[string]any
}

func (e *RequestError) Error() string { return e.Message }

// HTTPStatus is the status for the code: 400 for parameter errors, 409 for
// targets that conflict with the running version.
func (e *RequestError) HTTPStatus() int {
	switch e.Code {
	case CodeUnknownParam, CodeInvalidParam:
		return http.StatusBadRequest
	default:
		return http.StatusConflict
	}
}

func requestErrorf(code, format string, args ...any) *RequestError {
	return &RequestError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ParamValidator is implemented by executors that can check their
// parameters before a run is created. A *RequestError result carries the
// error code and HTTP status to report.
type ParamValidator interface {
	ValidateParams(params map[string]string) error
}

// CheckAllowedParams refuses any parameter not in allowed with
// CodeUnknownParam.
func CheckAllowedParams(params map[string]string, allowed []string) error {
	var unknown []string
	for k := range params {
		ok := false
		for _, a := range allowed {
			if k == a {
				ok = true
				break
			}
		}
		if !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return requestErrorf(CodeUnknownParam, "unknown parameter(s): %s (allowed: %s)",
		strings.Join(unknown, ", "), strings.Join(allowed, ", "))
}

// PendingRestart describes an installed update whose hub restart is about to
// be requested. It is passed to the before-restart hook.
type PendingRestart struct {
	From     string // running version ("" for a git build)
	To       string // installed release tag
	Strategy string // Installer.Name()
	Snapshot string // database snapshot path; "" when none could be taken (PostgreSQL)
	Channel  string // the installed release's channel
	Pin      string // PinLatest or the installed tag (the pin parameter)
	// FromPublishedAt is the publish date of a preview or stable From
	// (zero for a nightly, which carries its date, a git build, or when the
	// lookup failed). It goes into the update-state file for the target's
	// previous_safe computation.
	FromPublishedAt time.Time
}

// BeforeRestartFunc runs after the new binary is activated and before the
// hub is restarted. The runner implements it to persist the run (status
// "restarting", log, result) and the pending-update record, because the
// restart ends this process. An error aborts the restart and fails the run.
type BeforeRestartFunc func(ctx context.Context, p PendingRestart) error

// RestartingExecutor is implemented by executors whose last step restarts
// the hub. Runners that record runs call RunWithBeforeRestart so the record
// is written before the process exits.
type RestartingExecutor interface {
	MaintenanceExecutor
	RunWithBeforeRestart(ctx context.Context, logger io.Writer, params map[string]string, before BeforeRestartFunc) error
}

// BinaryUpdateOptions configures a BinaryUpdateExecutor. Zero values take
// the documented defaults.
type BinaryUpdateOptions struct {
	// GitHubRepo is the release repository (owner/name); default
	// DefaultGitHubRepo.
	GitHubRepo string
	// Installer installs and restarts. nil means unsupported.
	Installer Installer
	// Releases resolves a tag to its release; default GitHub's API for
	// GitHubRepo.
	Releases ReleaseSource
	// Catalog resolves latest:<channel> to the channel's newest installable
	// release. Default: a new Catalog for GitHubRepo over Releases (the hub
	// passes its shared one).
	Catalog CatalogSource
	// HTTPClient is the base client for API requests and asset downloads
	// (its redirect policy is replaced); default http.DefaultClient settings.
	HTTPClient *http.Client
	// CurrentVersion is the running version; default version.Version.
	CurrentVersion string
	// CurrentBuildTime dates a running git build for cross-channel
	// ordering; default version.BuildTime when it parses as RFC 3339.
	CurrentBuildTime time.Time
	// MaxDownloadBytes caps the tarball; default DefaultMaxDownloadBytes.
	MaxDownloadBytes int64
	// DownloadTimeout bounds resolve + download + verify; default
	// DefaultDownloadTimeout.
	DownloadTimeout time.Duration
	// Snapshotter takes the database snapshot before every update. nil
	// means snapshots are unsupported: upgrades go ahead without one and
	// older targets are refused (CodeDowngradeUnsupportedDriver).
	Snapshotter DBSnapshotter
	// DowngradeFloor is the oldest installable release; default
	// version.DowngradeFloor.
	DowngradeFloor string
}

// BinaryUpdateExecutor installs one verified release tag of GitHubRepo
// through an Installer:
//
//  1. validate params (see ValidateParams);
//  2. Installer.Preflight, before any network access;
//  3. resolve latest:<channel> through the release catalog, look the
//     release up with a single-tag API lookup and refuse the running
//     version, targets below the downgrade floor and older targets without
//     confirm_downgrade (checkTarget);
//  4. download the tarball and SHA256SUMS from the pinned release URL
//     (redirects only to *.githubusercontent.com, size and time capped) and
//     verify the tarball's own checksum line;
//  5. extract only `scion` and check `scion version --format json`;
//  6. snapshot the database (DBSnapshotter; skipped, with a log line, when
//     unsupported) and pass its path to the before-restart hook;
//  7. Installer.Stage, Installer.Activate, the before-restart hook,
//     Installer.Restart. If the hook or Restart fails, an installer that
//     implements Deactivator (symlink) is rolled back so nothing changed;
//     sudo-install cannot be, and its error says the new binary stays.
//
// Preflight, install, the hook and Restart each run under a short deadline
// derived from the run ctx, the download phase under DownloadTimeout.
//
// It writes no hub settings itself.
type BinaryUpdateExecutor struct {
	repo       string
	installer  Installer
	releases   ReleaseSource
	catalog    CatalogSource
	httpClient *http.Client
	current    string
	buildTime  time.Time
	maxBytes   int64
	timeout    time.Duration
	snapshots  DBSnapshotter
	floor      string

	// Per-step deadlines after the download phase.
	preflightTimeout time.Duration
	installTimeout   time.Duration
	hookTimeout      time.Duration
	restartTimeout   time.Duration
}

// Deadlines for the steps of an update outside the download phase.
const (
	preflightTimeout = 30 * time.Second
	installTimeout   = 2 * time.Minute
	hookTimeout      = 30 * time.Second
	restartTimeout   = 30 * time.Second
)

// NewBinaryUpdateExecutor returns a BinaryUpdateExecutor configured from opts.
func NewBinaryUpdateExecutor(opts BinaryUpdateOptions) *BinaryUpdateExecutor {
	e := &BinaryUpdateExecutor{
		repo:       opts.GitHubRepo,
		installer:  opts.Installer,
		releases:   opts.Releases,
		catalog:    opts.Catalog,
		httpClient: opts.HTTPClient,
		current:    opts.CurrentVersion,
		buildTime:  opts.CurrentBuildTime,
		maxBytes:   opts.MaxDownloadBytes,
		timeout:    opts.DownloadTimeout,
		snapshots:  opts.Snapshotter,
		floor:      opts.DowngradeFloor,
	}
	if e.repo == "" {
		e.repo = DefaultGitHubRepo
	}
	if e.installer == nil {
		e.installer = NewUnsupportedInstaller("no installer configured", "")
	}
	if e.releases == nil {
		gh := NewGitHubReleases(e.repo)
		if opts.HTTPClient != nil {
			c := *opts.HTTPClient
			c.Timeout = releaseAPITimeout
			gh.Client = &c
		}
		e.releases = gh
	}
	if e.catalog == nil {
		e.catalog = NewCatalog(CatalogOptions{Repo: e.repo, Lister: catalogLister(e.releases, e.repo)})
	}
	if e.current == "" {
		e.current = version.Version
	}
	if e.buildTime.IsZero() {
		if t, err := time.Parse(time.RFC3339, version.BuildTime); err == nil {
			e.buildTime = t
		}
	}
	if e.maxBytes <= 0 {
		e.maxBytes = DefaultMaxDownloadBytes
	}
	if e.timeout <= 0 {
		e.timeout = DefaultDownloadTimeout
	}
	if e.snapshots == nil {
		e.snapshots = NewUnsupportedDBSnapshotter("no database snapshotter is configured")
	}
	if e.floor == "" {
		e.floor = version.DowngradeFloor
	}
	e.preflightTimeout = preflightTimeout
	e.installTimeout = installTimeout
	e.hookTimeout = hookTimeout
	e.restartTimeout = restartTimeout
	return e
}

// ValidateParams implements ParamValidator. It needs no network, so a
// handler can call it before creating a run:
//   - only target_version, channel, pin and confirm_downgrade are accepted
//     (CodeUnknownParam);
//   - target_version is a release tag, or latest:<channel> for any release
//     channel (CodeInvalidParam);
//   - channel, when given, is the target's channel (CodeInvalidParam);
//   - pin, when given, is "latest" or the target_version tag; a
//     latest:<channel> target can only be pinned to "latest"
//     (CodeInvalidParam). An absent pin means "latest";
//   - confirm_downgrade, when given, is a release tag (CodeInvalidParam);
//   - a concrete target equal to the running version is CodeAlreadyRunning;
//     a nightly target older than the downgrade floor is
//     CodeBelowDowngradeFloor; an older one in the same channel needs
//     confirm_downgrade == target (see admitDowngrade).
//
// Cross-channel order and the floor of a preview or stable target need
// publish dates; CheckTarget and Run check them.
func (e *BinaryUpdateExecutor) ValidateParams(params map[string]string) error {
	if err := CheckAllowedParams(params, UpdateBinaryParams); err != nil {
		return err
	}
	target := params[ParamTargetVersion]
	if target == "" {
		return requestErrorf(CodeInvalidParam, "target_version is required")
	}
	channel := params[ParamChannel]
	pin := params[ParamPin]
	if c := params[ParamConfirmDowngrade]; c != "" && !update.IsReleaseTag(c) {
		return requestErrorf(CodeInvalidParam, "confirm_downgrade %q is not a release tag", c)
	}

	if ch, ok := strings.CutPrefix(target, LatestTargetPrefix); ok {
		if !slices.Contains(catalogChannels[:], ch) {
			return requestErrorf(CodeInvalidParam, "unknown channel %q in target_version %q", ch, target)
		}
		if channel != "" && channel != ch {
			return requestErrorf(CodeInvalidParam, "channel %q does not match target_version %q", channel, target)
		}
		if pin != "" && pin != PinLatest {
			return requestErrorf(CodeInvalidParam, "pin %q: a %s target can only be pinned to %q", pin, target, PinLatest)
		}
		return nil
	}

	if !update.IsReleaseTag(target) {
		return requestErrorf(CodeInvalidParam, "target_version %q is not a release tag", target)
	}
	if channel != "" && channel != update.DetectChannel(target) {
		return requestErrorf(CodeInvalidParam, "channel %q does not match target_version %q (channel %q)",
			channel, target, update.DetectChannel(target))
	}
	if pin != "" && pin != PinLatest && pin != target {
		return requestErrorf(CodeInvalidParam, "pin %q must be %q or the target_version %q", pin, PinLatest, target)
	}
	// Only what the tag strings decide; an undecidable floor or order is
	// left to CheckTarget and Run, which have the publish dates.
	if below, known := update.BelowFloor(e.floor, update.Ref{Version: target}); known && below {
		return e.belowFloorError(target)
	}
	switch c := update.Compare(e.current, target); {
	case c == update.ClassSame:
		return requestErrorf(CodeAlreadyRunning, "the hub is already running %s", target)
	case c.IsDowngrade():
		return e.admitDowngrade(target, c, params)
	}
	return nil
}

// pinParam is the pin recorded for an update: the pin parameter, PinLatest
// when it is absent.
func pinParam(params map[string]string) string {
	if pin := params[ParamPin]; pin != "" {
		return pin
	}
	return PinLatest
}

// belowFloorError refuses target as older than the downgrade floor.
func (e *BinaryUpdateExecutor) belowFloorError(target string) error {
	return &RequestError{
		Code: CodeBelowDowngradeFloor,
		Message: fmt.Sprintf("%s is older than the downgrade floor %s of the running version: "+
			"releases before it cannot run on this hub's database schema", target, e.floor),
		Details: map[string]any{"target": target, "floor": e.floor},
	}
}

// admitDowngrade decides whether an older target (class downgrade or
// switch-older) may be installed: only when the database can be
// snapshotted (CodeDowngradeUnsupportedDriver otherwise) and
// confirm_downgrade is the target tag (CodeDowngradeRequiresConfirmation,
// with the risk list, otherwise).
func (e *BinaryUpdateExecutor) admitDowngrade(target string, class update.Class, params map[string]string) error {
	st := e.snapshots.SnapshotStatus()
	if !st.Supported {
		return &RequestError{
			Code: CodeDowngradeUnsupportedDriver,
			Message: fmt.Sprintf("%s is older than the running version %s, and downgrades need a database snapshot, "+
				"which this hub cannot take (%s)", target, displayVersion(e.current), st.Reason),
			Details: map[string]any{"target": target, "class": string(class), "reason": st.Reason},
		}
	}
	if params[ParamConfirmDowngrade] == target {
		return nil
	}
	return &RequestError{
		Code: CodeDowngradeRequiresConfirmation,
		Message: fmt.Sprintf("%s is older than the running version %s; set confirm_downgrade to %q to install it",
			target, displayVersion(e.current), target),
		Details: DowngradeDetails(e.current, target, class, e.floor, st),
	}
}

// DowngradeDetails is the "details" object of a
// CodeDowngradeRequiresConfirmation refusal (design D4 §4): the target, the
// running version, the class, the floor, the snapshot directory and the
// risks to show before the operator types the target tag.
//
// snapshotPath is the directory the snapshot will be written to; the file
// (hub-<from>-<timestamp>.db) is named in the run log and result.
func DowngradeDetails(current, target string, class update.Class, floor string, st DBSnapshotStatus) map[string]any {
	running := displayVersion(current)
	order := fmt.Sprintf("%s is older than the running version %s.", target, running)
	if class == update.ClassSwitchOlder {
		order = fmt.Sprintf("%s (%s channel) was published before the running version %s.",
			target, update.DetectChannel(target), running)
	}
	risks := []string{
		order,
		fmt.Sprintf("The database schema is not migrated down: tables and columns added after %s are ignored by it, "+
			"and data that newer releases already removed (the decision_audits table, the templates.config "+
			"and templates.image columns) does not come back.", target),
		fmt.Sprintf("Harness bundles created by newer releases may be rejected by %s.", target),
		fmt.Sprintf("Settings keys that %s does not know may be dropped when it rewrites settings.yaml "+
			"(a copy is saved with the snapshot).", target),
		fmt.Sprintf("Broker plugins are not changed by this update; a plugin built for a newer release may fail with %s.", target),
		fmt.Sprintf("A database snapshot is written to %s before the update. Restoring it is a manual step: "+
			"stop the hub, copy the snapshot over the database, switch the release back, start the hub.", st.Dir),
	}
	return map[string]any{
		"target":       target,
		"current":      running,
		"class":        string(class),
		"floor":        floor,
		"snapshotPath": st.Dir,
		"risks":        risks,
	}
}

// Run implements MaintenanceExecutor. Without a before-restart hook the
// restart is requested right after activation; runners that record runs use
// RunWithBeforeRestart.
func (e *BinaryUpdateExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	return e.RunWithBeforeRestart(ctx, logger, params, nil)
}

// RunWithBeforeRestart implements RestartingExecutor.
func (e *BinaryUpdateExecutor) RunWithBeforeRestart(ctx context.Context, logger io.Writer, params map[string]string, before BeforeRestartFunc) error {
	log := logging.Subsystem("hub.maintenance.update-binary")

	if runtime.GOOS != "linux" {
		return fmt.Errorf("update-binary is only supported on Linux (requires systemd); current OS is %s", runtime.GOOS)
	}
	if err := e.ValidateParams(params); err != nil {
		return err
	}
	target := params[ParamTargetVersion]

	// ── Preflight: before any network access ────────────────────────────
	_, _ = fmt.Fprintf(logger, "==> Preflight (installer: %s)\n", e.installer.Name())
	preCtx, cancelPre := context.WithTimeout(ctx, e.preflightTimeout)
	err := e.installer.Preflight(preCtx)
	cancelPre()
	if err != nil {
		return fmt.Errorf("installer preflight failed (%s): %w", e.installer.Name(), err)
	}
	_, _ = fmt.Fprintln(logger, "Preflight passed.")

	dlCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	// ── Resolve the release ─────────────────────────────────────────────
	if ch, ok := strings.CutPrefix(target, LatestTargetPrefix); ok {
		resolved, err := e.resolveLatest(dlCtx, ch)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(logger, "Resolved %s to %s\n", target, resolved)
		target = resolved
	}
	_, _ = fmt.Fprintf(logger, "Current version: %s\nTarget version: %s\n", displayVersion(e.current), target)

	rel, err := e.releases.ReleaseByTag(dlCtx, target)
	if err != nil {
		return fmt.Errorf("look up release %s: %w", target, err)
	}
	if rel.Draft || rel.Tag != target {
		return fmt.Errorf("release %s is not a published release of %s", target, e.repo)
	}
	if err := e.checkTarget(dlCtx, target, rel, params); err != nil {
		return err
	}
	fromPublished := e.runningPublished(dlCtx)

	assetName := BinaryAssetName()
	binAsset, ok := rel.Asset(assetName)
	if !ok {
		return fmt.Errorf("release %s has no %s asset", target, assetName)
	}
	sumsAsset, ok := rel.Asset(ChecksumsAssetName)
	if !ok {
		return fmt.Errorf("release %s has no %s asset; refusing to install an unverifiable binary", target, ChecksumsAssetName)
	}
	for _, a := range []ReleaseAsset{binAsset, sumsAsset} {
		if err := CheckAssetURL(e.repo, target, a.URL); err != nil {
			return fmt.Errorf("release %s: %w", target, err)
		}
	}
	log.Info("Starting binary update", "from", e.current, "to", target, "installer", e.installer.Name())

	// ── Checksum, then download and verify ──────────────────────────────
	client := downloadClient(e.httpClient)
	_, _ = fmt.Fprintf(logger, "\n==> Fetching %s\n", ChecksumsAssetName)
	var sums bytes.Buffer
	if _, err := fetchCapped(dlCtx, client, sumsAsset.URL, maxChecksumsSize, &sums); err != nil {
		return fmt.Errorf("fetch %s: %w", ChecksumsAssetName, err)
	}
	expected, err := ExpectedChecksum(sums.Bytes(), assetName)
	if err != nil {
		return fmt.Errorf("checksum verification failed: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "scion-update-*")
	if err != nil {
		return fmt.Errorf("create temp directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	_, _ = fmt.Fprintf(logger, "\n==> Downloading %s\n", binAsset.URL)
	tarballPath := filepath.Join(tmpDir, assetName)
	actual, n, err := downloadHashed(dlCtx, client, binAsset.URL, tarballPath, e.maxBytes)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	_, _ = fmt.Fprintf(logger, "Downloaded %d bytes\n", n)
	if actual != expected {
		return fmt.Errorf("checksum mismatch for %q: expected %s, got %s", assetName, expected, actual)
	}
	_, _ = fmt.Fprintf(logger, "Checksum verified for %s: %s\n", assetName, actual)

	_, _ = fmt.Fprintf(logger, "\n==> Extracting and verifying binary\n")
	bin, err := ExtractScionBinary(tarballPath, tmpDir, logger)
	if err != nil {
		return fmt.Errorf("extraction failed: %w", err)
	}
	if err := VerifyScionBinary(dlCtx, bin, target, logger); err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}
	cancel()

	// ── Database snapshot, before anything changes ──────────────────────
	// On the run ctx (the overall backstop): VACUUM INTO of a large
	// database can take a while.
	snapshot, err := e.snapshot(ctx, logger)
	if err != nil {
		return err
	}

	// ── Install ─────────────────────────────────────────────────────────
	// Each remaining step has its own deadline, derived from the run ctx, so
	// a hung sudo (NSS/PAM lookup) or store write cannot wedge the run.
	_, _ = fmt.Fprintf(logger, "\n==> Installing %s (%s)\n", target, e.installer.Name())
	installCtx, cancelInstall := context.WithTimeout(ctx, e.installTimeout)
	staged, err := e.installer.Stage(installCtx, target, bin)
	if err != nil {
		cancelInstall()
		return fmt.Errorf("stage %s: %w", target, err)
	}
	err = e.installer.Activate(installCtx, staged)
	cancelInstall()
	if err != nil {
		return fmt.Errorf("activate %s: %w", target, err)
	}
	_, _ = fmt.Fprintf(logger, "Installed %s.\n", target)

	// ── Restart ─────────────────────────────────────────────────────────
	_, _ = fmt.Fprintf(logger, "\n==> Restarting hub: %s -> %s at %s\n",
		displayVersion(e.current), target, time.Now().UTC().Format(time.RFC3339))
	if before != nil {
		p := PendingRestart{
			From:            e.current,
			To:              target,
			Strategy:        e.installer.Name(),
			Snapshot:        snapshot,
			Channel:         update.DetectChannel(target),
			Pin:             pinParam(params),
			FromPublishedAt: fromPublished,
		}
		hookCtx, cancelHook := context.WithTimeout(ctx, e.hookTimeout)
		err := before(hookCtx, p)
		cancelHook()
		if err != nil {
			log.Error("Before-restart hook failed; not restarting", "error", err)
			return e.undoActivation(ctx, logger, target,
				fmt.Errorf("recording the restart failed, so the hub was not restarted: %w", err))
		}
	}
	restartCtx, cancelRestart := context.WithTimeout(ctx, e.restartTimeout)
	err = e.installer.Restart(restartCtx)
	cancelRestart()
	if err != nil {
		return e.undoActivation(ctx, logger, target, fmt.Errorf("restarting the hub failed: %w", err))
	}
	log.Info("Binary update installed, restart requested", "from", e.current, "to", target)
	_, _ = fmt.Fprintln(logger, "Restart requested.")
	return nil
}

// undoActivation is called when an activated update cannot be followed by a
// restart. Installers that implement Deactivator are rolled back, so the
// failed run changed nothing. Others (sudo-install keeps no backup) leave
// the new binary installed, and the error says it will run after the next
// restart.
func (e *BinaryUpdateExecutor) undoActivation(ctx context.Context, logger io.Writer, target string, cause error) error {
	d, ok := e.installer.(Deactivator)
	if !ok {
		return fmt.Errorf("%w; %s cannot be rolled back by the %s installer (no backup of the previous binary is kept): %s stays installed and will run after the hub's next restart",
			cause, target, e.installer.Name(), target)
	}
	// ctx is the run's context, not the hook's or Restart's (their own
	// deadlines never reach here). It may still have ended: the run's
	// overall deadline (the 20-minute update-binary backstop) or a
	// cancellation. The rollback must run regardless, so it is detached from
	// that cancellation, keeps ctx's values (logging, trace) and is bounded
	// on its own.
	undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.installTimeout)
	defer cancel()
	if err := d.Deactivate(undoCtx); err != nil {
		return fmt.Errorf("%w; rolling back %s also failed (%v): %s stays installed and will run after the hub's next restart",
			cause, target, err, target)
	}
	_, _ = fmt.Fprintf(logger, "Rolled back: the previous release is active again.\n")
	return fmt.Errorf("%w; %s was rolled back, nothing changed", cause, target)
}

// resolveLatest returns the newest installable release of channel in the
// release catalog. Its order against the running version is checked by
// checkTarget.
func (e *BinaryUpdateExecutor) resolveLatest(ctx context.Context, channel string) (string, error) {
	snap, err := e.catalog.Get(ctx, false)
	if err != nil {
		return "", fmt.Errorf("resolve latest %s release: %w", channel, err)
	}
	rel, ok := snap.Latest(channel)
	if !ok {
		return "", fmt.Errorf("no installable %s release is published", channel)
	}
	tag := rel.Tag
	if !update.IsReleaseTag(tag) || update.DetectChannel(tag) != channel {
		return "", fmt.Errorf("release catalog lists %q as latest %s, which is not a %s release tag", tag, channel, channel)
	}
	if update.Compare(e.current, tag) == update.ClassSame {
		return "", requestErrorf(CodeAlreadyRunning, "the hub is already running %s, the latest %s release", tag, channel)
	}
	return tag, nil
}

// checkTarget admits the resolved target release rel: it refuses a target
// below the downgrade floor (by publish date for another channel; an
// undecidable floor is refused), the running version, and an older target
// (downgrade or switch-older) that admitDowngrade does not admit. A
// cross-channel order that cannot be decided (no publish date for the
// running release) counts as switch-older, so it needs the confirmation.
func (e *BinaryUpdateExecutor) checkTarget(ctx context.Context, target string, rel *Release, params map[string]string) error {
	tgt := update.Ref{Version: target, Date: rel.PublishedAt}
	switch below, known := update.BelowFloor(e.floor, tgt); {
	case !known:
		return &RequestError{
			Code: CodeBelowDowngradeFloor,
			Message: fmt.Sprintf("cannot tell whether %s is older than the downgrade floor %s (no publish date); refusing",
				target, e.floor),
			Details: map[string]any{"target": target, "floor": e.floor},
		}
	case below:
		return e.belowFloorError(target)
	}

	cur := update.Ref{Version: e.current, Date: e.buildTime}
	if update.IsReleaseTag(e.current) && update.DetectChannel(e.current) != update.DetectChannel(target) {
		// Another channel: order by the running release's publish date.
		cur.Date = time.Time{}
		if curRel, err := e.releases.ReleaseByTag(ctx, e.current); err == nil {
			cur.Date = curRel.PublishedAt
		}
	}
	switch class := update.CompareRefs(cur, tgt); {
	case class == update.ClassSame:
		return requestErrorf(CodeAlreadyRunning, "the hub is already running %s", target)
	case class == update.ClassUnknown:
		return e.admitDowngrade(target, update.ClassSwitchOlder, params)
	case class.IsDowngrade():
		return e.admitDowngrade(target, class, params)
	}
	return nil
}

// CheckTarget runs every check Run makes before its download, without
// preflight, download or install: the parameters, latest:<channel>
// resolution, the release lookup, the downgrade floor and the
// confirmation of an older target. A handler calls it before creating a
// run so a refusal (a *RequestError, such as the 409
// downgrade_requires_confirmation with its risk list) is answered
// synchronously. Any other error (GitHub unreachable) is not a refusal; Run
// repeats every check.
func (e *BinaryUpdateExecutor) CheckTarget(ctx context.Context, params map[string]string) error {
	if err := e.ValidateParams(params); err != nil {
		return err
	}
	target := params[ParamTargetVersion]
	if ch, ok := strings.CutPrefix(target, LatestTargetPrefix); ok {
		resolved, err := e.resolveLatest(ctx, ch)
		if err != nil {
			return err
		}
		target = resolved
	}
	rel, err := e.releases.ReleaseByTag(ctx, target)
	if err != nil {
		return fmt.Errorf("look up release %s: %w", target, err)
	}
	if rel.Draft || rel.Tag != target {
		return fmt.Errorf("release %s is not a published release of %s", target, e.repo)
	}
	return e.checkTarget(ctx, target, rel, params)
}

// snapshot takes the database snapshot before an update and returns its
// path. A hub that cannot snapshot (PostgreSQL) reaches here only for an
// upgrade (admitDowngrade refuses older targets): it goes ahead without one
// and says so in the run log. A failed snapshot fails the update before
// anything is installed.
func (e *BinaryUpdateExecutor) snapshot(ctx context.Context, logger io.Writer) (string, error) {
	_, _ = fmt.Fprintf(logger, "\n==> Database snapshot\n")
	st := e.snapshots.SnapshotStatus()
	if !st.Supported {
		logging.Subsystem("hub.maintenance.update-binary").Warn("Updating without a database snapshot", "reason", st.Reason)
		_, _ = fmt.Fprintf(logger, "No database snapshot taken: %s. Upgrades go ahead without one; downgrades are refused.\n", st.Reason)
		return "", nil
	}
	path, err := e.snapshots.Snapshot(ctx, e.current, logger)
	if err != nil {
		return "", fmt.Errorf("database snapshot failed, nothing was installed: %w", err)
	}
	return path, nil
}

// runningPublished returns the publish date of the running preview or
// stable release, zero for a nightly (its tag carries the date), a git
// build or a failed lookup. It is best-effort: it only dates the
// update-state file's from.
func (e *BinaryUpdateExecutor) runningPublished(ctx context.Context) time.Time {
	if !update.IsReleaseTag(e.current) || update.DetectChannel(e.current) == "nightly" {
		return time.Time{}
	}
	rel, err := e.releases.ReleaseByTag(ctx, e.current)
	if err != nil || rel == nil {
		return time.Time{}
	}
	return rel.PublishedAt
}

// TargetChecker is implemented by executors that can check a request
// against remote state (release lookups) before a run is created; see
// BinaryUpdateExecutor.CheckTarget.
type TargetChecker interface {
	CheckTarget(ctx context.Context, params map[string]string) error
}

// downloadHashed downloads u to path (at most maxBytes) and returns the
// SHA-256 of what was written.
func downloadHashed(ctx context.Context, client *http.Client, u, path string, maxBytes int64) (string, int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return "", 0, fmt.Errorf("create %s: %w", path, err)
	}
	h := sha256.New()
	n, err := fetchCapped(ctx, client, u, maxBytes, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func displayVersion(v string) string {
	if v == "" {
		return "(git build)"
	}
	return v
}

// IsRequestError reports whether err is (or wraps) a *RequestError and
// returns it.
func IsRequestError(err error) (*RequestError, bool) {
	var re *RequestError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}
