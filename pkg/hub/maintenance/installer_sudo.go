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
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SudoStagingName is the file, in the maintenance repo, where sudo-install
// stages the binary. It is the path the starter-hub sudoers rule allows
// installing from (scripts/starter-hub/gce-start-hub.sh), shared with
// rebuild-server.
const SudoStagingName = "scion.rebuild"

// sudoInstaller installs through the existing starter-hub sudoers rules:
//
//	install -m 755 <RepoPath>/scion.rebuild <BinaryDest>
//	systemctl restart --no-block <service>   (newer hosts)
//	systemctl restart <service>              (all hosts)
//
// Preflight checks each exact command with `sudo -n -l <argv>`, which tests
// whether a rule matches without running it. No other sudo command is used.
type sudoInstaller struct {
	repoPath    string
	binaryDest  string
	serviceName string
	runner      CommandRunner
	// restartGrace is how long the plain restart is watched for an early
	// failure; default plainRestartGrace.
	restartGrace time.Duration

	mu      sync.Mutex
	noBlock bool // the --no-block restart rule exists (set by Preflight)
}

func newSudoInstaller(opts DetectOptions) *sudoInstaller {
	return &sudoInstaller{
		repoPath:     opts.RepoPath,
		binaryDest:   opts.BinaryDest,
		serviceName:  opts.ServiceName,
		runner:       opts.Runner,
		restartGrace: plainRestartGrace,
	}
}

// plainRestartGrace bounds how long Restart watches a plain
// `systemctl restart` for an early failure.
const plainRestartGrace = 10 * time.Second

func (s *sudoInstaller) stagingPath() string { return filepath.Join(s.repoPath, SudoStagingName) }

func (s *sudoInstaller) installArgv() []string {
	return []string{"install", "-m", "755", s.stagingPath(), s.binaryDest}
}

func (s *sudoInstaller) restartArgv(noBlock bool) []string {
	if noBlock {
		return []string{"systemctl", "restart", "--no-block", s.serviceName}
	}
	return []string{"systemctl", "restart", s.serviceName}
}

// Name implements Installer.
func (s *sudoInstaller) Name() string { return StrategySudoInstall }

// Status implements Installer.
func (s *sudoInstaller) Status() InstallerStatus {
	return InstallerStatus{Strategy: StrategySudoInstall, Supported: true}
}

// sudoAllowed reports whether sudo -n -l accepts the exact argv.
func (s *sudoInstaller) sudoAllowed(ctx context.Context, argv []string) bool {
	args := append([]string{"-n", "-l"}, argv...)
	_, err := s.runner.Run(ctx, "sudo", args...)
	return err == nil
}

// Preflight implements Installer: the repo path is a writable directory, the
// install rule matches, and at least one restart rule matches (the
// --no-block one is preferred).
func (s *sudoInstaller) Preflight(ctx context.Context) error {
	if s.repoPath == "" {
		return fmt.Errorf("no maintenance repo path configured")
	}
	fi, err := os.Stat(s.repoPath)
	if err != nil || !fi.IsDir() {
		return fmt.Errorf("maintenance repo path %s is not a directory", s.repoPath)
	}
	if !isWritableDir(s.repoPath) {
		return fmt.Errorf("maintenance repo path %s is not writable by the hub user", s.repoPath)
	}
	if !s.sudoAllowed(ctx, s.installArgv()) {
		return fmt.Errorf("no sudoers rule allows `%s`", strings.Join(s.installArgv(), " "))
	}
	switch {
	case s.sudoAllowed(ctx, s.restartArgv(true)):
		s.setNoBlock(true)
	case s.sudoAllowed(ctx, s.restartArgv(false)):
		s.setNoBlock(false)
	default:
		return fmt.Errorf("no sudoers rule allows `%s`", strings.Join(s.restartArgv(false), " "))
	}
	return nil
}

func (s *sudoInstaller) setNoBlock(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noBlock = v
}

func (s *sudoInstaller) useNoBlock() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.noBlock
}

// Stage implements Installer: bin is copied to <RepoPath>/scion.rebuild via
// a temporary file and a rename, so a half-written file is never installed.
func (s *sudoInstaller) Stage(_ context.Context, _ string, bin string) (string, error) {
	dst := s.stagingPath()
	tmp := dst + ".tmp"
	_ = os.Remove(tmp)
	if err := copyExecutable(bin, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename %s into place: %w", tmp, err)
	}
	return dst, nil
}

// Activate implements Installer: sudo -n install -m 755 <staged> <dest>.
// install(1) writes a new file and renames it over the destination, so a
// failure leaves the old binary in place.
func (s *sudoInstaller) Activate(ctx context.Context, staged string) error {
	if filepath.Clean(staged) != s.stagingPath() {
		return fmt.Errorf("staged binary %s is not %s", staged, s.stagingPath())
	}
	args := append([]string{"-n"}, s.installArgv()...)
	if out, err := s.runner.Run(ctx, "sudo", args...); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("sudo %s: %w", strings.Join(s.installArgv(), " "), ctx.Err())
		}
		if detail := strings.TrimSpace(string(out)); detail != "" {
			return fmt.Errorf("sudo %s: %s", strings.Join(s.installArgv(), " "), detail)
		}
		return fmt.Errorf("sudo %s: %w", strings.Join(s.installArgv(), " "), err)
	}
	return nil
}

// Restart implements Installer. With the --no-block rule systemctl returns
// once the restart job is queued. With only the plain rule the command
// blocks while systemd stops this process, so it is started and watched for
// plainRestartGrace: only a normal non-zero exit in that window (nothing was
// restarted) fails Restart.
func (s *sudoInstaller) Restart(ctx context.Context) error {
	if s.useNoBlock() {
		args := append([]string{"-n"}, s.restartArgv(true)...)
		if out, err := s.runner.Run(ctx, "sudo", args...); err != nil {
			return fmt.Errorf("sudo %s: %v %s", strings.Join(s.restartArgv(true), " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	args := append([]string{"-n"}, s.restartArgv(false)...)
	done, err := s.runner.Start("sudo", args...)
	if err != nil {
		return fmt.Errorf("sudo %s: %w", strings.Join(s.restartArgv(false), " "), err)
	}
	// The plain restart blocks while systemd stops the unit. With the
	// default KillMode=control-group systemd SIGTERMs every process in the
	// hub's cgroup, this sudo/systemctl child included, so a child killed by
	// a signal (or still running after the grace period) means the restart
	// is under way. Only a normal exit with a non-zero status (no matching
	// sudoers rule, systemctl refusing) means nothing was restarted.
	timer := time.NewTimer(s.restartGrace)
	defer timer.Stop()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if err != nil && errors.As(err, &ee) && ee.Exited() {
			return fmt.Errorf("sudo %s: %w", strings.Join(s.restartArgv(false), " "), err)
		}
		return nil
	case <-timer.C:
		return nil
	case <-ctx.Done():
		// The child was started and has not failed so far; as with the
		// grace timer, assume the restart is under way rather than fail a
		// run whose unit may already be stopping.
		return nil
	}
}

// Retained implements Installer: sudo-install keeps no releases.
func (s *sudoInstaller) Retained() []InstalledRelease { return nil }
