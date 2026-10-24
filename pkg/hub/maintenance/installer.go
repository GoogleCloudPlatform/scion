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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Installer strategy names, as reported by Installer.Name and
// InstallerStatus.Strategy.
const (
	StrategySymlink     = "symlink"
	StrategySudoInstall = "sudo-install"
	StrategyUnsupported = "unsupported"
)

// Defaults for DetectOptions.
const (
	DefaultInstallRoot = config.DefaultMaintenanceInstallRoot
	DefaultBinaryDest  = "/usr/local/bin/scion"
	DefaultServiceName = "scion-hub"
)

// MigrateLayoutHint is the operator action shown when a single-node VM hub
// cannot update itself because it still uses the pre-/opt/scion layout.
const MigrateLayoutHint = "Run `scripts/single-node-vm/deploy.sh migrate-layout` to move this hub to the /opt/scion layout, then retry."

// Installer installs a verified release binary on this host and restarts
// the hub. The strategy is chosen once by Detect, from how the running
// binary is installed.
//
// The update-binary executor calls, in order: Preflight (before any
// download), Stage, Activate, then (after its before-restart hook) Restart.
type Installer interface {
	// Name returns the strategy: StrategySymlink, StrategySudoInstall or
	// StrategyUnsupported.
	Name() string
	// Preflight checks, without changing anything, that Stage, Activate and
	// Restart can succeed. It runs before any download. The error explains
	// what is missing.
	Preflight(ctx context.Context) error
	// Stage copies the verified binary bin, for release tag, to where
	// Activate expects it, and returns that location.
	Stage(ctx context.Context, tag, bin string) (staged string, err error)
	// Activate makes the staged binary the one the service starts. It is
	// atomic, or leaves the previous binary in place on failure.
	Activate(ctx context.Context, staged string) error
	// Restart restarts the hub. The current process is expected to receive
	// SIGTERM shortly after (or during) the call.
	Restart(ctx context.Context) error
	// Retained lists the releases kept on disk for local rollback. Only the
	// symlink strategy retains releases; others return nil.
	Retained() []InstalledRelease
	// Status reports the strategy and, when it is not usable, why and how to
	// fix it. It reflects the result of Detect.
	Status() InstallerStatus
}

// Deactivator is implemented by installers that can undo their last
// Activate. The update-binary executor calls it when the before-restart hook
// or Restart fails, so the failed run leaves the old release active. The symlink
// installer implements it; sudo-install cannot (no sudoers rule allows a
// backup of the installed binary).
type Deactivator interface {
	Deactivate(ctx context.Context) error
}

// InstallerStatus is the installer section of the update-status API.
type InstallerStatus struct {
	Strategy    string `json:"strategy"`
	Supported   bool   `json:"supported"`
	Reason      string `json:"reason"`
	MigrateHint string `json:"migrateHint"`
}

// InstalledRelease is a release directory retained by the symlink installer.
type InstalledRelease struct {
	Tag         string    `json:"tag"`
	Path        string    `json:"path"`
	InstalledAt time.Time `json:"installedAt"`
	Current     bool      `json:"current"`
	Previous    bool      `json:"previous"`
}

// CommandRunner runs host commands for the installers. Tests inject a fake
// so that no sudo or systemctl command ever runs.
type CommandRunner interface {
	// Run runs the command to completion and returns its combined output.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// Start starts the command without waiting for it, for commands that
	// terminate this process (systemctl restart of our own unit). done
	// receives the command's exit result once it exits: nil on success,
	// otherwise the *exec.ExitError (or equivalent) from Wait, so callers can
	// tell a non-zero exit from death by a signal.
	Start(name string, args ...string) (done <-chan error, err error)
}

// ExecRunner is the CommandRunner that runs real commands.
type ExecRunner struct{}

// Run implements CommandRunner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Start implements CommandRunner. The child restarts the service this
// process belongs to, so the caller does not block on it; it is reaped in
// the background and its exit result sent on done.
func (ExecRunner) Start(name string, args ...string) (<-chan error, error) {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done, nil
}

// DetectOptions configures Detect. Zero values take the documented defaults.
type DetectOptions struct {
	// InstallRoot is the symlink layout root (server.maintenance.install_root);
	// default DefaultInstallRoot.
	InstallRoot string
	// RepoPath is the maintenance repository checkout. The sudo-install
	// strategy is considered only when it is set.
	RepoPath string
	// BinaryDest is where sudo-install installs the binary; default
	// DefaultBinaryDest.
	BinaryDest string
	// ServiceName is the systemd unit; default DefaultServiceName.
	ServiceName string

	// Executable returns the running binary's path; default os.Executable.
	Executable func() (string, error)
	// Runner runs host commands (sudo); default ExecRunner.
	Runner CommandRunner
	// Signal asks this process to shut down so systemd restarts it; default
	// sends SIGTERM to the current process.
	Signal func() error
	// UID is the hub's uid, which must own the install root's current
	// symlink; default os.Getuid. Tests override it.
	UID func() int
	// GOOS overrides runtime.GOOS (tests only).
	GOOS string
}

func (o DetectOptions) withDefaults() DetectOptions {
	if o.InstallRoot == "" {
		o.InstallRoot = DefaultInstallRoot
	}
	if o.BinaryDest == "" {
		o.BinaryDest = DefaultBinaryDest
	}
	if o.ServiceName == "" {
		o.ServiceName = DefaultServiceName
	}
	if o.Executable == nil {
		o.Executable = os.Executable
	}
	if o.Runner == nil {
		o.Runner = ExecRunner{}
	}
	if o.Signal == nil {
		o.Signal = signalSelf
	}
	if o.UID == nil {
		o.UID = os.Getuid
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	return o
}

// Detect picks the Installer strategy for this host: symlink if the running
// binary lives in the install root's releases and the layout is usable, else
// sudo-install if a repo path is configured and the exact sudoers rules exist,
// else unsupported (with the reasons and a migration hint).
//
// Blanket sudo is never used on its own: without a repo path the hub is
// unsupported even if the hub user may run any command as root.
func Detect(ctx context.Context, opts DetectOptions) Installer {
	opts = opts.withDefaults()
	if opts.GOOS != "linux" {
		return &unsupportedInstaller{status: InstallerStatus{
			Strategy: StrategyUnsupported,
			Reason:   fmt.Sprintf("self-update requires Linux with systemd; this host is %s", opts.GOOS),
		}}
	}

	sym := newSymlinkInstaller(opts)
	symErr := sym.Preflight(ctx)
	if symErr == nil {
		return sym
	}

	var reasons []string
	reasons = append(reasons, "symlink layout: "+symErr.Error())

	if opts.RepoPath == "" {
		reasons = append(reasons, "sudo-install: no maintenance repo path configured")
	} else {
		sudo := newSudoInstaller(opts)
		sudoErr := sudo.Preflight(ctx)
		if sudoErr == nil {
			return sudo
		}
		reasons = append(reasons, "sudo-install: "+sudoErr.Error())
	}

	return &unsupportedInstaller{status: InstallerStatus{
		Strategy:    StrategyUnsupported,
		Reason:      strings.Join(reasons, "; "),
		MigrateHint: MigrateLayoutHint,
	}}
}

// ErrInstallerUnsupported is wrapped by every error of the unsupported
// installer.
var ErrInstallerUnsupported = errors.New("self-update is not supported on this host")

// unsupportedInstaller is the Installer for hosts where the hub cannot
// replace its own binary. Every action fails, starting with Preflight, so
// nothing is downloaded.
type unsupportedInstaller struct {
	status InstallerStatus
}

// NewUnsupportedInstaller returns an Installer that refuses every action with
// reason (and hint, when set). Useful where detection is not possible.
func NewUnsupportedInstaller(reason, hint string) Installer {
	return &unsupportedInstaller{status: InstallerStatus{
		Strategy: StrategyUnsupported, Reason: reason, MigrateHint: hint,
	}}
}

func (u *unsupportedInstaller) err() error {
	msg := u.status.Reason
	if u.status.MigrateHint != "" {
		msg += ". " + u.status.MigrateHint
	}
	return fmt.Errorf("%w: %s", ErrInstallerUnsupported, msg)
}

func (u *unsupportedInstaller) Name() string                           { return StrategyUnsupported }
func (u *unsupportedInstaller) Preflight(context.Context) error        { return u.err() }
func (u *unsupportedInstaller) Activate(context.Context, string) error { return u.err() }
func (u *unsupportedInstaller) Restart(context.Context) error          { return u.err() }
func (u *unsupportedInstaller) Retained() []InstalledRelease           { return nil }
func (u *unsupportedInstaller) Status() InstallerStatus                { return u.status }
func (u *unsupportedInstaller) Stage(context.Context, string, string) (string, error) {
	return "", u.err()
}
